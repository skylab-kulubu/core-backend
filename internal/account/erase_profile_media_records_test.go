package account_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/account"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/transit"
	"github.com/skylab-kulubu/core-backend/internal/transit/transittest"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// The erase_profile_media step erases the uploads anonymize_core recorded
// (media redesign ticket 07) as well as the profile picture.

// recordsFixture is a person being erased, core's Postgres store and the
// public and private buckets.
type recordsFixture struct {
	pool            *pgxpool.Pool
	users           *user.PostgresStore
	media           *media.PostgresStore
	public, private *media.MemoryBlob
	buckets         media.Buckets
	subject         uuid.UUID
	request         user.DeletionRequest
	now             time.Time
	// stepTimeout is the worker's StepTimeout; zero keeps its default.
	stepTimeout time.Duration
}

func newRecordsFixture(t *testing.T) *recordsFixture {
	t.Helper()
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	f := &recordsFixture{
		pool: pool, users: user.NewPostgresStore(pool), media: media.NewPostgresStore(pool),
		public: media.NewMemoryBlob(), private: media.NewMemoryBlob(), subject: uuid.New(),
	}
	bao := transittest.NewServer(t)
	f.buckets = media.Buckets{Public: f.public, Private: media.NewPrivateStorage(f.private, transit.New(bao.Config()))}
	if _, _, err := user.NewService(f.users).Ensure(ctx, f.subject, user.Profile{Email: "ada@example.test"}); err != nil {
		t.Fatal(err)
	}
	return f
}

// answerFile is a private Answer file of the person, as a private upload
// stores it.
func (f *recordsFixture) answerFile(t *testing.T) media.Media {
	t.Helper()
	ctx := context.Background()
	item, err := f.media.Create(ctx, media.Media{
		Name: "Ada_Lovelace_CV.pdf", Type: "application/pdf", Kind: media.KindFile,
		Key: media.PrivateObjectKey("files/" + uuid.NewString()), UploadedBy: f.subject, Purpose: media.PurposeAnswerFile,
		Visibility: media.VisibilityPrivate, Encryption: &media.Encryption{Algorithm: "aes-256-gcm-chunked-v1", WrappedKey: "vault:v1:AAAA", KeyVersion: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.private.Put(ctx, item.Key, []byte("sealed"), media.BlobMetadata{}); err != nil {
		t.Fatal(err)
	}
	return item
}

// requestDeletion queues the person's erasure, with the access marker
// confirmed, and returns a worker over blobs.
func (f *recordsFixture) requestDeletion(t *testing.T, eraser account.MediaEraser) *account.Worker {
	t.Helper()
	request, err := f.users.RequestDeletion(context.Background(), f.subject, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.request, f.now = request, request.NextAttemptAt
	confirmDeletionProjection(t, f.users, request, f.now)
	return account.NewWorkerWithConfiguredWaits(f.users, successfulIdentity{}, account.WorkerConfig{
		Services: erasedServices(),
		Now:      func() time.Time { return f.now }, Lease: time.Minute, AccessBlocker: &accountBlockWriter{},
		StepTimeout: f.stepTimeout,
	}, eraser)
}

// attempts is the request's spent attempts and when it is claimed next.
func (f *recordsFixture) attempts(t *testing.T) (int, time.Time) {
	t.Helper()
	request, err := f.users.DeletionRequest(context.Background(), f.subject)
	if err != nil {
		t.Fatal(err)
	}
	return request.AttemptCount, request.NextAttemptAt
}

func (f *recordsFixture) checkpointed(t *testing.T, step user.DeletionStep) bool {
	t.Helper()
	var done bool
	if err := f.pool.QueryRow(context.Background(), `SELECT EXISTS (SELECT 1 FROM account_deletion_steps WHERE request_id = $1 AND step = $2)`,
		f.request.ID, step).Scan(&done); err != nil {
		t.Fatal(err)
	}
	return done
}

func (f *recordsFixture) recorded(t *testing.T) []uuid.UUID {
	t.Helper()
	rows, err := f.pool.Query(context.Background(), `SELECT media_id FROM account_deletion_media WHERE request_id = $1 ORDER BY media_id`, f.request.ID)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

func (f *recordsFixture) mediaForDeletion(t *testing.T) []uuid.UUID {
	t.Helper()
	ids, err := f.users.MediaForDeletion(context.Background(), f.request.ID)
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

func (f *recordsFixture) completed(t *testing.T) bool {
	t.Helper()
	request, err := f.users.DeletionRequest(context.Background(), f.subject)
	if err != nil {
		t.Fatal(err)
	}
	return request.Status == user.DeletionRequestCompleted
}

// purged reports whether the Media is recorded purged and its object is in
// neither bucket.
func (f *recordsFixture) purged(t *testing.T, item media.Media) bool {
	t.Helper()
	stored, err := f.media.GetIncludingDeleted(context.Background(), item.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, public := f.public.Get(item.Key)
	_, private := f.private.Get(item.Key)
	return stored.BlobPurgedAt != nil && !public && !private
}

// failingDelete is the buckets with the deletion of one object failing
// once. With lost, the object is deleted first, as when storage deleted it
// and its answer was lost; the error names the object, as a storage
// client's error may.
type failingDelete struct {
	media.Buckets
	key  string
	lost bool
	// names is more the error names: the Media, its file.
	names string

	mu     sync.Mutex
	failed bool
}

func (b *failingDelete) Delete(ctx context.Context, key string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if key != b.key || b.failed {
		return b.Buckets.Delete(ctx, key)
	}
	b.failed = true
	if b.lost {
		if err := b.Buckets.Delete(ctx, key); err != nil {
			return err
		}
	}
	return errors.New(`DeleteObject "https://r2.example.test/media/` + key + `" (` + b.names + `): connection reset`)
}

// countingEraser counts the Media erase_profile_media hands the eraser.
type countingEraser struct {
	*media.ImmediateBlobEraser
	mu    sync.Mutex
	calls int
}

func (e *countingEraser) EnsureErased(ctx context.Context, id uuid.UUID, at time.Time) error {
	e.mu.Lock()
	e.calls++
	e.mu.Unlock()
	return e.ImmediateBlobEraser.EnsureErased(ctx, id, at)
}

// A person with no profile picture: their recorded Answer file is purged
// from the private bucket in the erase_profile_media step. A storage failure
// after the object went fails the step with no address, id or subject in the
// error; the rerun finds the object gone, counts that as erased and
// checkpoints the step.
func TestEraseProfileMediaPurgesARecordedAnswerFileWithoutAProfilePicture(t *testing.T) {
	f := newRecordsFixture(t)
	answer := f.answerFile(t)
	blobs := &failingDelete{Buckets: f.buckets, key: answer.Key, lost: true, names: answer.ID.String() + " " + answer.Name}
	worker := f.requestDeletion(t, media.NewImmediateBlobEraser(f.media, blobs))

	worked, err := worker.RunOnce(context.Background())
	if !worked || !errors.Is(err, media.ErrRecordedMediaNotErased) {
		t.Fatalf("first pass worked=%v err=%v", worked, err)
	}
	for _, private := range []string{answer.ID.String(), answer.Key, f.subject.String(), answer.Name} {
		if strings.Contains(err.Error(), private) {
			t.Fatalf("error names %q: %v", private, err)
		}
	}
	if !f.checkpointed(t, user.DeletionStepAnonymizeCore) || f.checkpointed(t, user.DeletionStepEraseProfile) {
		t.Fatal("the failed erase_profile_media was checkpointed, or anonymize_core was not")
	}
	if _, ok := f.private.Get(answer.Key); ok {
		t.Fatal("the object is still in the private bucket")
	}
	if got := f.mediaForDeletion(t); !slices.Equal(got, []uuid.UUID{answer.ID}) {
		t.Fatalf("media for deletion after the failed pass %v, want the Answer file", got)
	}
	if spent, _ := f.attempts(t); spent != 1 {
		t.Fatalf("a pass that erased nothing spent %d attempts, want 1", spent)
	}

	if worked, err := worker.RunOnce(context.Background()); !worked || err != nil {
		t.Fatalf("rerun worked=%v err=%v", worked, err)
	}
	if !f.checkpointed(t, user.DeletionStepEraseProfile) || !f.completed(t) {
		t.Fatal("the rerun did not checkpoint erase_profile_media and complete")
	}
	if !f.purged(t, answer) {
		t.Fatal("the Answer file is not purged")
	}
	if got := f.recorded(t); len(got) != 0 {
		t.Fatalf("records %v left", got)
	}
}

// A step stopped halfway resumes with what is left: the Media purged before
// the failure lost its record in its own purge, so the rerun is handed only
// the other one.
func TestEraseProfileMediaResumesAnInterruptedStepWithWhatIsLeft(t *testing.T) {
	f := newRecordsFixture(t)
	answers := []media.Media{f.answerFile(t), f.answerFile(t)}
	slices.SortFunc(answers, func(a, b media.Media) int { return slices.Compare(a.ID[:], b.ID[:]) })
	first, second := answers[0], answers[1]
	blobs := &failingDelete{Buckets: f.buckets, key: second.Key}
	worker := f.requestDeletion(t, media.NewImmediateBlobEraser(f.media, blobs))

	if worked, err := worker.RunOnce(context.Background()); !worked || !errors.Is(err, media.ErrRecordedMediaNotErased) {
		t.Fatalf("first pass worked=%v err=%v", worked, err)
	}
	if !f.purged(t, first) || f.purged(t, second) {
		t.Fatal("the first pass did not purge exactly the first Answer file")
	}
	if got := f.mediaForDeletion(t); !slices.Equal(got, []uuid.UUID{second.ID}) {
		t.Fatalf("media for deletion after the interruption %v, want only %v", got, second.ID)
	}
	if spent, next := f.attempts(t); spent != 0 || !next.Equal(f.now.Add(30*time.Second)) {
		t.Fatalf("a pass that erased something spent %d attempts, next at %v; want 0, in 30 s", spent, next)
	}

	f.now = f.now.Add(30 * time.Second)
	if worked, err := worker.RunOnce(context.Background()); !worked || err != nil {
		t.Fatalf("rerun worked=%v err=%v", worked, err)
	}
	if !f.purged(t, first) || !f.purged(t, second) || !f.completed(t) {
		t.Fatal("the rerun did not purge both Answer files and complete")
	}
	if got := f.recorded(t); len(got) != 0 {
		t.Fatalf("records %v left", got)
	}
}

// A profile picture club content also uses (a legacy picture that is an
// Event's cover too) is kept: it is not the profile erasure's
// (profile_media_id stays empty) but a record like the Answer file beside
// it, which the rule keeps as club content. It loses its uploader and name;
// the Answer file is purged.
func TestEraseProfileMediaKeepsASharedProfilePictureAsClubContent(t *testing.T) {
	f := newRecordsFixture(t)
	ctx := context.Background()
	picture, err := f.media.Create(ctx, media.Media{
		Name: "Ada_Lovelace.png", Type: "image/png", Kind: media.KindImage, Key: "images/" + uuid.NewString(), UploadedBy: f.subject,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.public.Put(ctx, picture.Key, []byte("picture"), media.BlobMetadata{ContentType: picture.Type}); err != nil {
		t.Fatal(err)
	}
	if _, err := user.NewService(f.users).SetProfilePicture(ctx, f.subject, picture.ID, picture.Key); err != nil {
		t.Fatal(err)
	}
	if _, err := event.NewPostgresStore(f.pool).Create(ctx, event.Event{Name: "Shared cover", Location: "YTÜ", OwnerTeam: "WEBLAB", CoverImageID: &picture.ID}); err != nil {
		t.Fatal(err)
	}
	answer := f.answerFile(t)
	worker := f.requestDeletion(t, media.NewImmediateBlobEraser(f.media, f.buckets))

	if err := f.users.AnonymizeAccount(ctx, f.subject, f.now, nil); err != nil {
		t.Fatal(err)
	}
	both := []uuid.UUID{answer.ID, picture.ID}
	slices.SortFunc(both, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
	if got := f.recorded(t); !slices.Equal(got, both) {
		t.Fatalf("recorded %v, want the Answer file and the shared picture", got)
	}
	if got := f.mediaForDeletion(t); !slices.Equal(got, both) {
		t.Fatalf("media for deletion %v: a shared picture is not the profile erasure's, only a record", got)
	}

	if worked, err := worker.RunOnce(ctx); !worked || err != nil || !f.completed(t) {
		t.Fatalf("worked=%v err=%v completed=%v", worked, err, f.completed(t))
	}
	stored, err := f.media.GetIncludingDeleted(ctx, picture.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := f.public.Get(picture.Key); !ok || stored.BlobPurgedAt != nil || stored.DeletedAt != nil {
		t.Fatalf("the shared picture was not kept: %+v", stored)
	}
	if stored.UploadedBy != uuid.Nil || stored.Name != "" {
		t.Fatalf("the shared picture kept uploader %v and name %q", stored.UploadedBy, stored.Name)
	}
	var profile *uuid.UUID
	if err := f.pool.QueryRow(ctx, `SELECT profile_media_id FROM account_deletion_requests WHERE id = $1`, f.request.ID).Scan(&profile); err != nil || profile != nil {
		t.Fatalf("profile_media_id %v err %v, want none", profile, err)
	}
	if !f.purged(t, answer) {
		t.Fatal("the Answer file is not purged")
	}
}

// The current profile picture is the profile erasure's (profile_media_id)
// and is never recorded, even when its purpose is profile_picture, a
// personal purpose; an earlier picture of theirs is recorded. The step hands
// each to the eraser once and purges both.
func TestEraseProfileMediaNeverRecordsTheCurrentProfilePicture(t *testing.T) {
	f := newRecordsFixture(t)
	ctx := context.Background()
	var pictures []media.Media
	for range 2 {
		picture, err := f.media.Create(ctx, media.Media{
			Name: "Ada_Lovelace.png", Type: "image/png", Kind: media.KindImage, Key: "images/" + uuid.NewString(),
			UploadedBy: f.subject, Purpose: media.PurposeProfilePicture,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := f.public.Put(ctx, picture.Key, []byte("picture"), media.BlobMetadata{ContentType: picture.Type}); err != nil {
			t.Fatal(err)
		}
		if _, err := user.NewService(f.users).SetProfilePicture(ctx, f.subject, picture.ID, picture.Key); err != nil {
			t.Fatal(err)
		}
		pictures = append(pictures, picture)
	}
	earlier, current := pictures[0], pictures[1]
	eraser := &countingEraser{ImmediateBlobEraser: media.NewImmediateBlobEraser(f.media, f.buckets)}
	worker := f.requestDeletion(t, eraser)

	if err := f.users.AnonymizeAccount(ctx, f.subject, f.now, nil); err != nil {
		t.Fatal(err)
	}
	if got := f.recorded(t); !slices.Equal(got, []uuid.UUID{earlier.ID}) {
		t.Fatalf("recorded %v, want only the earlier picture %v", got, earlier.ID)
	}
	var both int
	if err := f.pool.QueryRow(ctx, `
		SELECT count(*) FROM account_deletion_media record
		JOIN account_deletion_requests request ON request.id = record.request_id
		WHERE record.media_id = request.profile_media_id`).Scan(&both); err != nil || both != 0 {
		t.Fatalf("%d Media are both profile_media_id and recorded (err %v)", both, err)
	}
	if got := f.mediaForDeletion(t); !slices.Equal(got, []uuid.UUID{current.ID, earlier.ID}) {
		t.Fatalf("media for deletion %v, want the current picture then the earlier one", got)
	}

	if worked, err := worker.RunOnce(ctx); !worked || err != nil || !f.completed(t) {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	if eraser.calls != 2 || !f.purged(t, current) || !f.purged(t, earlier) {
		t.Fatalf("eraser calls %d; current purged %v, earlier purged %v", eraser.calls, f.purged(t, current), f.purged(t, earlier))
	}
}

// With neither a profile picture nor a record the step has nothing to hand
// the eraser: the list is empty, and the eraser is not called.
func TestEraseProfileMediaCallsNoEraserWithNothingToErase(t *testing.T) {
	f := newRecordsFixture(t)
	eraser := &countingEraser{ImmediateBlobEraser: media.NewImmediateBlobEraser(f.media, f.buckets)}
	worker := f.requestDeletion(t, eraser)

	if worked, err := worker.RunOnce(context.Background()); !worked || err != nil || !f.completed(t) {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	if !f.checkpointed(t, user.DeletionStepEraseProfile) || eraser.calls != 0 {
		t.Fatalf("erase_profile_media checkpointed %v with %d eraser calls, want 0", f.checkpointed(t, user.DeletionStepEraseProfile), eraser.calls)
	}
}

// stalledDelete is the buckets with one object's deletion hanging, once,
// until the step's time is up.
type stalledDelete struct {
	media.Buckets
	key string

	mu      sync.Mutex
	stalled bool
}

func (b *stalledDelete) Delete(ctx context.Context, key string) error {
	b.mu.Lock()
	stall := key == b.key && !b.stalled
	b.stalled = b.stalled || stall
	b.mu.Unlock()
	if stall {
		<-ctx.Done()
		return ctx.Err()
	}
	return b.Buckets.Delete(ctx, key)
}

// A person with many files: the step's time runs out after the first of
// three. The pass erased something, so the worker gives the attempt back and
// comes again in 30 seconds; that pass erases the other two.
func TestEraseProfileMediaGivesTheAttemptBackWhenItErasedSome(t *testing.T) {
	f := newRecordsFixture(t)
	f.stepTimeout = 2 * time.Second
	answers := []media.Media{f.answerFile(t), f.answerFile(t), f.answerFile(t)}
	slices.SortFunc(answers, func(a, b media.Media) int { return slices.Compare(a.ID[:], b.ID[:]) })
	blobs := &stalledDelete{Buckets: f.buckets, key: answers[1].Key}
	worker := f.requestDeletion(t, media.NewImmediateBlobEraser(f.media, blobs))

	worked, err := worker.RunOnce(context.Background())
	if !worked || err == nil {
		t.Fatalf("timed-out pass worked=%v err=%v", worked, err)
	}
	for _, answer := range answers {
		for _, private := range []string{answer.ID.String(), answer.Key, answer.Name} {
			if strings.Contains(err.Error(), private) {
				t.Fatalf("error names %q: %v", private, err)
			}
		}
	}
	if !f.purged(t, answers[0]) || f.purged(t, answers[1]) || f.purged(t, answers[2]) {
		t.Fatal("the timed-out pass did not purge exactly the first Answer file")
	}
	if spent, next := f.attempts(t); spent != 0 || !next.Equal(f.now.Add(30*time.Second)) {
		t.Fatalf("the timed-out pass spent %d attempts, next at %v; want 0, in 30 s", spent, next)
	}
	if worked, err := worker.RunOnce(context.Background()); worked || err != nil {
		t.Fatalf("the request was claimed before its 30 s: worked=%v err=%v", worked, err)
	}

	f.now = f.now.Add(30 * time.Second)
	if worked, err := worker.RunOnce(context.Background()); !worked || err != nil {
		t.Fatalf("next pass worked=%v err=%v", worked, err)
	}
	for _, answer := range answers {
		if !f.purged(t, answer) {
			t.Fatalf("%s is not purged", answer.ID)
		}
	}
	if !f.completed(t) {
		t.Fatal("the request did not complete")
	}
}

// A profile picture purged in an earlier pass is not offered again, so it
// cannot pass for progress: while a recorded Media keeps failing, each pass
// after the first erases nothing and spends its attempt.
func TestEraseProfileMediaSpendsTheAttemptWhenItErasedNothing(t *testing.T) {
	f := newRecordsFixture(t)
	ctx := context.Background()
	picture, err := f.media.Create(ctx, media.Media{
		Name: "Ada_Lovelace.png", Type: "image/png", Kind: media.KindImage, Key: "images/" + uuid.NewString(),
		UploadedBy: f.subject, Purpose: media.PurposeProfilePicture,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := user.NewService(f.users).SetProfilePicture(ctx, f.subject, picture.ID, picture.Key); err != nil {
		t.Fatal(err)
	}
	answer := f.answerFile(t)
	worker := f.requestDeletion(t, media.NewImmediateBlobEraser(f.media, &brokenDelete{Buckets: f.buckets, key: answer.Key}))

	if worked, err := worker.RunOnce(ctx); !worked || err == nil {
		t.Fatalf("first pass worked=%v err=%v", worked, err)
	}
	if spent, _ := f.attempts(t); spent != 0 || !f.purged(t, picture) {
		t.Fatalf("first pass purged the picture %v and spent %d attempts; want purged, 0", f.purged(t, picture), spent)
	}
	if got := f.mediaForDeletion(t); !slices.Equal(got, []uuid.UUID{answer.ID}) {
		t.Fatalf("media for deletion %v, want only the Answer file: the purged picture is done", got)
	}

	f.now = f.now.Add(30 * time.Second)
	if worked, err := worker.RunOnce(ctx); !worked || !errors.Is(err, media.ErrRecordedMediaNotErased) {
		t.Fatalf("second pass worked=%v err=%v", worked, err)
	}
	if spent, _ := f.attempts(t); spent != 1 {
		t.Fatalf("a pass that erased nothing spent %d attempts, want 1", spent)
	}
}

// brokenDelete is the buckets with one object's deletion always failing.
type brokenDelete struct {
	media.Buckets
	key string
}

func (b *brokenDelete) Delete(ctx context.Context, key string) error {
	if key == b.key {
		return errors.New("object storage unavailable")
	}
	return b.Buckets.Delete(ctx, key)
}

// The step's error goes to the worker's log, so it names no Media, object or
// file whatever failed underneath: here a stored record core cannot read,
// whose own error names the Media.
func TestEraseProfileMediaErrorNamesNoMedia(t *testing.T) {
	f := newRecordsFixture(t)
	ctx := context.Background()
	picture, err := f.media.Create(ctx, media.Media{
		Name: "Ada_Lovelace.png", Type: "image/png", Kind: media.KindImage, Key: "images/" + uuid.NewString(), UploadedBy: f.subject,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := user.NewService(f.users).SetProfilePicture(ctx, f.subject, picture.ID, picture.Key); err != nil {
		t.Fatal(err)
	}
	worker := f.requestDeletion(t, media.NewImmediateBlobEraser(f.media, f.buckets))
	if err := f.users.AnonymizeAccount(ctx, f.subject, f.now, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.media.Restore(ctx, picture.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE media SET size_objects = '{"card": 5}' WHERE id = $1`, picture.ID); err != nil {
		t.Fatal(err)
	}

	worked, err := worker.RunOnce(ctx)
	if !worked || err == nil {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	for _, private := range []string{picture.ID.String(), picture.Key, "Ada_Lovelace", f.subject.String()} {
		if strings.Contains(err.Error(), private) {
			t.Fatalf("error names %q: %v", private, err)
		}
	}
}

// legacyUpload is a legacy image of the person, stored with its object.
func (f *recordsFixture) legacyUpload(t *testing.T) media.Media {
	t.Helper()
	ctx := context.Background()
	item, err := f.media.Create(ctx, media.Media{
		Name: "Ada_Lovelace.png", Type: "image/png", Kind: media.KindImage, Key: "images/" + uuid.NewString(), UploadedBy: f.subject,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.public.Put(ctx, item.Key, []byte("image"), media.BlobMetadata{ContentType: item.Type}); err != nil {
		t.Fatal(err)
	}
	return item
}

// attachToCMSPage is a CMS page using the Media, through its Media
// attachment only.
func (f *recordsFixture) attachToCMSPage(t *testing.T, item media.Media) {
	t.Helper()
	if _, _, err := f.media.Attach(context.Background(), media.Attachment{
		MediaID: item.ID, Owner: media.Owner{Service: authz.ProductCMS, Type: "page", ID: "skylab-site:anasayfa"}, Role: media.RoleCMSImage,
	}); err != nil {
		t.Fatal(err)
	}
}

// A profile picture club content uses only through a Media attachment (a
// legacy picture a CMS page holds) is shared like one an Event uses: it is
// not archived, the request does not stop at erase_profile_media, and the
// file stays without its uploader and name.
func TestEraseProfileMediaKeepsAProfilePictureACMSPageUses(t *testing.T) {
	f := newRecordsFixture(t)
	ctx := context.Background()
	picture := f.legacyUpload(t)
	if _, err := user.NewService(f.users).SetProfilePicture(ctx, f.subject, picture.ID, picture.Key); err != nil {
		t.Fatal(err)
	}
	f.attachToCMSPage(t, picture)
	worker := f.requestDeletion(t, media.NewImmediateBlobEraser(f.media, f.buckets))

	if worked, err := worker.RunOnce(ctx); !worked || err != nil || !f.completed(t) {
		t.Fatalf("worked=%v err=%v completed=%v", worked, err, f.completed(t))
	}
	stored, err := f.media.GetIncludingDeleted(ctx, picture.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := f.public.Get(picture.Key); !ok || stored.DeletedAt != nil || stored.BlobPurgedAt != nil {
		t.Fatalf("the picture a CMS page uses was not kept: %+v", stored)
	}
	if stored.UploadedBy != uuid.Nil || stored.Name != "" {
		t.Fatalf("the kept picture has uploader %v and name %q", stored.UploadedBy, stored.Name)
	}
}

// A legacy upload nothing used when anonymize_core recorded it, but that a
// CMS page uses by the time erase_profile_media runs, is club content after
// all (decision E1, read again under the purge's locks): its record goes
// without a purge, the file stays, and the request completes past the
// completion guard.
func TestEraseProfileMediaKeepsARecordedLegacyUploadUsedSince(t *testing.T) {
	f := newRecordsFixture(t)
	ctx := context.Background()
	upload := f.legacyUpload(t)
	worker := f.requestDeletion(t, media.NewImmediateBlobEraser(f.media, f.buckets))
	if err := f.users.AnonymizeAccount(ctx, f.subject, f.now, nil); err != nil {
		t.Fatal(err)
	}
	if got := f.recorded(t); !slices.Equal(got, []uuid.UUID{upload.ID}) {
		t.Fatalf("recorded %v, want the unused legacy upload", got)
	}
	f.attachToCMSPage(t, upload)

	if worked, err := worker.RunOnce(ctx); !worked || err != nil || !f.completed(t) {
		t.Fatalf("worked=%v err=%v completed=%v", worked, err, f.completed(t))
	}
	if got := f.recorded(t); len(got) != 0 {
		t.Fatalf("records %v left", got)
	}
	stored, err := f.media.GetIncludingDeleted(ctx, upload.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := f.public.Get(upload.Key); !ok || stored.DeletedAt != nil || stored.BlobPurgedAt != nil || stored.BlobPurgeStartedAt != nil {
		t.Fatalf("the legacy upload a CMS page uses was not kept: %+v", stored)
	}
	if stored.UploadedBy != uuid.Nil || stored.Name != "" {
		t.Fatalf("the kept upload has uploader %v and name %q", stored.UploadedBy, stored.Name)
	}
}

// A profile picture belongs to the person whose current picture it is, not
// to its uploader: the one the erased person uploaded for someone else stays
// that person's picture, without the uploader's name.
func TestEraseProfileMediaKeepsSomeoneElsesProfilePicture(t *testing.T) {
	f := newRecordsFixture(t)
	ctx := context.Background()
	other := uuid.New()
	if _, _, err := user.NewService(f.users).Ensure(ctx, other, user.Profile{Email: "grace@example.test"}); err != nil {
		t.Fatal(err)
	}
	picture, err := f.media.Create(ctx, media.Media{
		Name: "Ada_Lovelace_for_Grace.png", Type: "image/png", Kind: media.KindImage, Key: "images/" + uuid.NewString(),
		UploadedBy: f.subject, Purpose: media.PurposeProfilePicture,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.public.Put(ctx, picture.Key, []byte("picture"), media.BlobMetadata{ContentType: picture.Type}); err != nil {
		t.Fatal(err)
	}
	if _, err := user.NewService(f.users).SetProfilePicture(ctx, other, picture.ID, picture.Key); err != nil {
		t.Fatal(err)
	}
	worker := f.requestDeletion(t, media.NewImmediateBlobEraser(f.media, f.buckets))

	if worked, err := worker.RunOnce(ctx); !worked || err != nil || !f.completed(t) {
		t.Fatalf("worked=%v err=%v completed=%v", worked, err, f.completed(t))
	}
	stored, err := f.media.GetIncludingDeleted(ctx, picture.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := f.public.Get(picture.Key); !ok || stored.BlobPurgedAt != nil || stored.DeletedAt != nil {
		t.Fatalf("someone else's profile picture was not kept: %+v", stored)
	}
	if stored.UploadedBy != uuid.Nil || stored.Name != "" {
		t.Fatalf("the kept picture has uploader %v and name %q", stored.UploadedBy, stored.Name)
	}
	if got := f.recorded(t); len(got) != 0 {
		t.Fatalf("records %v left", got)
	}
}

// A legacy upload only a Skyforms answer uses is the person's own (E1: an
// answer is a personal use), so it is purged like an Answer file.
func TestEraseProfileMediaPurgesALegacyUploadOnlyAnAnswerUses(t *testing.T) {
	f := newRecordsFixture(t)
	ctx := context.Background()
	upload := f.legacyUpload(t)
	if _, _, err := f.media.Attach(ctx, media.Attachment{
		MediaID: upload.ID, Owner: media.Owner{Service: authz.ProductForms, Type: "response", ID: uuid.NewString()}, Role: media.RoleFormsAnswer,
	}); err != nil {
		t.Fatal(err)
	}
	worker := f.requestDeletion(t, media.NewImmediateBlobEraser(f.media, f.buckets))

	if worked, err := worker.RunOnce(ctx); !worked || err != nil || !f.completed(t) {
		t.Fatalf("worked=%v err=%v completed=%v", worked, err, f.completed(t))
	}
	if !f.purged(t, upload) {
		t.Fatal("the legacy upload only an answer uses was not purged")
	}
}

// slowMetadata is the buckets taking a while to rewrite an object's
// metadata, as R2 does for a copy.
type slowMetadata struct {
	media.Buckets
	delay time.Duration
}

func (b slowMetadata) SetMetadata(ctx context.Context, key string, meta media.BlobMetadata) error {
	select {
	case <-time.After(b.delay):
	case <-ctx.Done():
		return ctx.Err()
	}
	return b.Buckets.SetMetadata(ctx, key, meta)
}

// A person with fifty uploads, most of them club content whose object
// metadata must lose their name: a pass ends when the step's time is up,
// gives its attempt back and comes again in 30 seconds, until one pass
// finishes. Then no record is left, and the request completes past the
// completion guard.
func TestEraseProfileMediaFinishesFiftyUploadsOverSeveralPasses(t *testing.T) {
	f := newRecordsFixture(t)
	f.stepTimeout = time.Second
	ctx := context.Background()
	var posters, answers []media.Media
	for range 45 {
		poster, err := f.media.Create(ctx, media.Media{
			Name: "Ada_Lovelace_poster.svg", Type: "image/svg+xml", Kind: media.KindImage, Key: "images/" + uuid.NewString() + ".svg",
			UploadedBy: f.subject, Purpose: media.PurposeEventCover,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := f.public.Put(ctx, poster.Key, []byte("<svg/>"), media.ServingMetadata(poster.Type, poster.Name)); err != nil {
			t.Fatal(err)
		}
		posters = append(posters, poster)
	}
	for range 5 {
		answers = append(answers, f.answerFile(t))
	}
	worker := f.requestDeletion(t, media.NewImmediateBlobEraser(f.media, slowMetadata{Buckets: f.buckets, delay: 60 * time.Millisecond}))

	passes := 0
	for !f.completed(t) {
		if passes++; passes > 20 {
			t.Fatal("no pass finished the step")
		}
		worked, err := worker.RunOnce(ctx)
		if !worked {
			t.Fatalf("pass %d was not claimed", passes)
		}
		if err == nil {
			continue
		}
		if spent, next := f.attempts(t); spent != 0 || !next.Equal(f.now.Add(30*time.Second)) {
			t.Fatalf("pass %d (%v) spent %d attempts, next at %v; want 0, in 30 s", passes, err, spent, next)
		}
		f.now = f.now.Add(30 * time.Second)
	}
	if passes < 2 {
		t.Fatalf("one pass erased all fifty uploads; the step timeout did not cut it")
	}
	if got := f.recorded(t); len(got) != 0 {
		t.Fatalf("%d records left", len(got))
	}
	for _, poster := range posters {
		if meta, _ := f.public.Metadata(poster.Key); meta.ContentDisposition != "attachment" {
			t.Fatalf("a kept poster is served with disposition %q", meta.ContentDisposition)
		}
	}
	for _, answer := range answers {
		if !f.purged(t, answer) {
			t.Fatal("an Answer file is not purged")
		}
	}
}

// A current profile picture club content also uses (a legacy SVG an Event
// has as its cover) is not the profile erasure's, so anonymize_core records
// it with the other uploads (profile_media_id stays empty) and the rule keeps
// it as club content: the file stays, its object downloads under its key
// instead of the person's file name, and its record goes.
func TestEraseProfileMediaStripsTheNameFromASharedProfilePicture(t *testing.T) {
	f := newRecordsFixture(t)
	ctx := context.Background()
	picture, err := f.media.Create(ctx, media.Media{
		Name: "Ada_Lovelace_portrait.svg", Type: "image/svg+xml", Kind: media.KindImage, Key: "images/" + uuid.NewString() + ".svg", UploadedBy: f.subject,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.public.Put(ctx, picture.Key, []byte("<svg/>"), media.ServingMetadata(picture.Type, picture.Name)); err != nil {
		t.Fatal(err)
	}
	if _, err := user.NewService(f.users).SetProfilePicture(ctx, f.subject, picture.ID, picture.Key); err != nil {
		t.Fatal(err)
	}
	if _, err := event.NewPostgresStore(f.pool).Create(ctx, event.Event{Name: "Shared cover", Location: "YTÜ", OwnerTeam: "WEBLAB", CoverImageID: &picture.ID}); err != nil {
		t.Fatal(err)
	}
	worker := f.requestDeletion(t, media.NewImmediateBlobEraser(f.media, f.buckets))

	if err := f.users.AnonymizeAccount(ctx, f.subject, f.now, nil); err != nil {
		t.Fatal(err)
	}
	if got := f.recorded(t); !slices.Equal(got, []uuid.UUID{picture.ID}) {
		t.Fatalf("recorded %v, want the shared picture", got)
	}
	if got := f.mediaForDeletion(t); !slices.Equal(got, []uuid.UUID{picture.ID}) {
		t.Fatalf("media for deletion %v, want the shared picture once, as a record", got)
	}

	if worked, err := worker.RunOnce(ctx); !worked || err != nil || !f.completed(t) {
		t.Fatalf("worked=%v err=%v completed=%v", worked, err, f.completed(t))
	}
	stored, err := f.media.GetIncludingDeleted(ctx, picture.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := f.public.Get(picture.Key); !ok || stored.BlobPurgedAt != nil || stored.DeletedAt != nil {
		t.Fatalf("the shared picture was not kept: %+v", stored)
	}
	if meta, _ := f.public.Metadata(picture.Key); meta.ContentDisposition != "attachment" || meta.ContentType != "image/svg+xml" {
		t.Fatalf("the shared picture is served as %q, %q; want image/svg+xml, attachment", meta.ContentType, meta.ContentDisposition)
	}
	if got := f.recorded(t); len(got) != 0 {
		t.Fatalf("records %v left", got)
	}
}

// erase_staged_uploads' error goes to the worker's log too: a staged object
// whose deletion fails, with a storage error naming it, leaves no key in it.
func TestEraseStagedUploadsErrorNamesNoObject(t *testing.T) {
	f := newRecordsFixture(t)
	ctx := context.Background()
	key := "files/" + uuid.NewString()
	if err := f.media.StageUpload(ctx, key, f.subject, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := f.public.Put(ctx, key, []byte("staged"), media.BlobMetadata{}); err != nil {
		t.Fatal(err)
	}
	worker := f.requestDeletion(t, media.NewImmediateBlobEraser(f.media, &brokenDeleteNamingKey{Buckets: f.buckets, key: key}))

	worked, err := worker.RunOnce(ctx)
	if !worked || !errors.Is(err, media.ErrStagedUploadNotErased) {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	if strings.Contains(err.Error(), key) || strings.Contains(err.Error(), f.subject.String()) {
		t.Fatalf("error names the object or the subject: %v", err)
	}
}

// brokenDeleteNamingKey is the buckets failing one object's deletion with an
// error that names it, as a storage client's may.
type brokenDeleteNamingKey struct {
	media.Buckets
	key string
}

func (b *brokenDeleteNamingKey) Delete(ctx context.Context, key string) error {
	if key == b.key {
		return errors.New(`DeleteObject "https://r2.example.test/media/` + key + `": connection reset`)
	}
	return b.Buckets.Delete(ctx, key)
}

// Club content another purge has claimed (its archive window is over) keeps
// that claim: the erasure strips the person's name and lets its record go,
// and the archive purge finishes what it started.
func TestEraseProfileMediaLeavesAnotherPurgesClaimOnClubContent(t *testing.T) {
	f := newRecordsFixture(t)
	ctx := context.Background()
	poster, err := f.media.Create(ctx, media.Media{
		Name: "Ada_Lovelace_poster.svg", Type: "image/svg+xml", Kind: media.KindImage, Key: "images/" + uuid.NewString() + ".svg",
		UploadedBy: f.subject, Purpose: media.PurposeEventCover,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.public.Put(ctx, poster.Key, []byte("<svg/>"), media.ServingMetadata(poster.Type, poster.Name)); err != nil {
		t.Fatal(err)
	}
	claimedAt := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
	if _, err := f.pool.Exec(ctx, `UPDATE media SET deleted_at = $2, blob_purge_started_at = $2 WHERE id = $1`, poster.ID, claimedAt); err != nil {
		t.Fatal(err)
	}
	worker := f.requestDeletion(t, media.NewImmediateBlobEraser(f.media, f.buckets))

	if worked, err := worker.RunOnce(ctx); !worked || err != nil || !f.completed(t) {
		t.Fatalf("worked=%v err=%v completed=%v", worked, err, f.completed(t))
	}
	stored, err := f.media.GetIncludingDeleted(ctx, poster.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.BlobPurgeStartedAt == nil || !stored.BlobPurgeStartedAt.Equal(claimedAt) {
		t.Fatalf("the archive purge's claim became %v, want %v kept", stored.BlobPurgeStartedAt, claimedAt)
	}
	if meta, _ := f.public.Metadata(poster.Key); meta.ContentDisposition != "attachment" {
		t.Fatalf("the claimed poster is served with disposition %q", meta.ContentDisposition)
	}
	if got := f.recorded(t); len(got) != 0 {
		t.Fatalf("records %v left", got)
	}
}
