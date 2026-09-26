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
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/transit"
	"github.com/skylab-kulubu/core-backend/internal/transit/transittest"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// The erase_profile_media step erases the personal Media anonymize_core
// recorded (media redesign ticket 07) as well as the profile picture.

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
	return account.NewWorker(f.users, successfulIdentity{}, account.WorkerConfig{
		Services: erasedServices(),
		Now:      func() time.Time { return f.now }, Lease: time.Minute, AccessBlocker: &accountBlockWriter{},
	}, eraser)
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
	return errors.New(`DeleteObject "https://r2.example.test/media/` + key + `": connection reset`)
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
	blobs := &failingDelete{Buckets: f.buckets, key: answer.Key, lost: true}
	worker := f.requestDeletion(t, media.NewImmediateBlobEraser(f.media, blobs))

	worked, err := worker.RunOnce(context.Background())
	if !worked || !errors.Is(err, media.ErrPersonalMediaNotErased) {
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

	if worked, err := worker.RunOnce(context.Background()); !worked || !errors.Is(err, media.ErrPersonalMediaNotErased) {
		t.Fatalf("first pass worked=%v err=%v", worked, err)
	}
	if !f.purged(t, first) || f.purged(t, second) {
		t.Fatal("the first pass did not purge exactly the first Answer file")
	}
	if got := f.mediaForDeletion(t); !slices.Equal(got, []uuid.UUID{second.ID}) {
		t.Fatalf("media for deletion after the interruption %v, want only %v", got, second.ID)
	}

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
// Event's cover too) keeps today's rule: the reference-aware purge keeps it,
// it loses its uploader and name, and profile_media_id goes. It never mixes
// with the records, which are purged beside it.
func TestEraseProfileMediaKeepsASharedProfilePictureApartFromTheRecords(t *testing.T) {
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
	if got := f.recorded(t); !slices.Equal(got, []uuid.UUID{answer.ID}) {
		t.Fatalf("recorded %v, want only the Answer file", got)
	}
	if got := f.mediaForDeletion(t); !slices.Equal(got, []uuid.UUID{answer.ID}) {
		t.Fatalf("media for deletion %v: a shared picture is not the profile erasure's", got)
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
	if profile, err := f.users.ProfileMediaForDeletion(ctx, f.request.ID); err != nil || profile != nil {
		t.Fatalf("profile media for deletion %v err %v, want none", profile, err)
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
