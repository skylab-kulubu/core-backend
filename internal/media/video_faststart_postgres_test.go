package media_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/faststart"
	"github.com/skylab-kulubu/core-backend/internal/faststart/mp4test"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/media/s3test"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// faststartDatabase is core's database and a fake R2 bucket, with a clock
// the tests move.
type faststartDatabase struct {
	pool     *pgxpool.Pool
	store    *media.PostgresStore
	r2       *media.R2
	fake     *s3test.Server
	uploader uuid.UUID
	now      time.Time
}

func newFaststartDatabase(t *testing.T) *faststartDatabase {
	t.Helper()
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	uploader := uuid.New()
	if _, _, err := user.NewService(user.NewPostgresStore(pool)).Ensure(ctx, uploader, user.Profile{Email: "video@example.test"}); err != nil {
		t.Fatal(err)
	}
	r2, fake := multipartR2(t)
	return &faststartDatabase{pool: pool, store: media.NewPostgresStore(pool), r2: r2, fake: fake, uploader: uploader, now: time.Now().UTC()}
}

// video stores an Event video as a Direct upload's completion leaves it: a
// pending Media at videos/<uuid>.mp4, served to play.
func (d *faststartDatabase) video(t *testing.T, file []byte) media.Media {
	t.Helper()
	ctx := context.Background()
	key := "videos/" + uuid.NewString() + ".mp4"
	if err := d.r2.Put(ctx, key, file, media.ServingMetadataFor(media.PurposeVideo, "video/mp4", "açılış.mp4")); err != nil {
		t.Fatal(err)
	}
	expires := d.now.Add(24 * time.Hour)
	created, err := d.store.Create(ctx, media.Media{
		Name: "açılış.mp4", Type: "video/mp4", Kind: media.KindFile, Key: key, Size: int64(len(file)),
		UploadedBy: d.uploader, Purpose: media.PurposeVideo, Status: media.StatusPending, ExpiresAt: &expires,
		Visibility: media.VisibilityPublic, ServingPolicyApplied: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return created
}

func (d *faststartDatabase) worker(t *testing.T, storage media.FaststartStorage, now func() time.Time) *media.FaststartWorker {
	t.Helper()
	w, err := media.NewFaststartWorker(media.FaststartWorkerConfig{Store: d.store, Storage: storage, Now: now, PartSize: 5 << 20})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func (d *faststartDatabase) clock() time.Time { return d.now }

func faststartPass(t *testing.T, w *media.FaststartWorker) media.FaststartReport {
	t.Helper()
	report, err := w.Pass(context.Background(), func(err error) { t.Logf("faststart: %v", err) })
	if err != nil {
		t.Fatal(err)
	}
	return report
}

// state is the Media's faststart state: "" while it waits.
func (d *faststartDatabase) state(t *testing.T, id uuid.UUID) string {
	t.Helper()
	var state string
	if err := d.pool.QueryRow(context.Background(), `SELECT COALESCE(video_faststart, '') FROM media WHERE id = $1`, id).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}

func (d *faststartDatabase) get(t *testing.T, id uuid.UUID) media.Media {
	t.Helper()
	got, err := d.store.GetIncludingDeleted(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// videoKeys are the objects stored for the video named in key
// (videos/<name>.mp4 and videos/<name>.fs.mp4).
func (d *faststartDatabase) videoKeys(key string) []string {
	name, _, _ := strings.Cut(strings.TrimSuffix(key, ".mp4"), ".fs.")
	var out []string
	for _, k := range d.fake.Keys("media") {
		if strings.HasPrefix(k, name+".") {
			out = append(out, k)
		}
	}
	return out
}

// copyOf reports whether key is a faststart copy of the video at original
// (videos/<uuid>.fs.<claim>.mp4).
func copyOf(original, key string) bool {
	return strings.HasPrefix(key, strings.TrimSuffix(original, ".mp4")+".fs.") && strings.HasSuffix(key, ".mp4")
}

// movedKey is the key v's Media points at, which must be a faststart copy
// of its original.
func (d *faststartDatabase) movedKey(t *testing.T, v media.Media) string {
	t.Helper()
	got := d.get(t, v.ID)
	if !copyOf(v.Key, got.Key) {
		t.Fatalf("Media at %s, not a faststart copy of %s", got.Key, v.Key)
	}
	return got.Key
}

// A video whose moov comes after its media data is rewritten with its moov
// in front into videos/<uuid>.fs.mp4, served to play as the original was,
// and the Media points there. Its original stays an hour, for a player that
// loaded its old address, then goes; the Media is then done.
func TestPostgresFaststartMovesAVideosMoovInFront(t *testing.T) {
	d := newFaststartDatabase(t)
	m := mp4test.TwoTracks()
	file, _ := m.Build()
	v := d.video(t, file)
	w := d.worker(t, d.r2, d.clock)

	if report := faststartPass(t, w); report.Rewritten != 1 {
		t.Fatalf("pass %+v, want the video rewritten", report)
	}
	got := d.get(t, v.ID)
	if !copyOf(v.Key, got.Key) || got.Size != int64(len(file)) {
		t.Fatalf("Media at %s of %d bytes, want a copy of %s", got.Key, got.Size, v.Key)
	}
	stored, ok := d.fake.Object("media", got.Key)
	if !ok || stored.ContentType != "video/mp4" || stored.ContentDisposition != "" {
		t.Fatalf("faststart copy %q %q (found %v), want video/mp4 inline", stored.ContentType, stored.ContentDisposition, ok)
	}
	mp4test.AssertSamePlayback(t, m, stored.Data)
	if _, ok := d.fake.Object("media", v.Key); !ok || d.state(t, v.ID) != "moved" {
		t.Fatalf("the original went at once (state %q)", d.state(t, v.ID))
	}
	if report := faststartPass(t, w); report.Rewritten+report.Finished != 0 {
		t.Fatalf("a pass within the hour %+v", report)
	}

	d.now = d.now.Add(media.FaststartOriginalGrace + time.Second)
	if report := faststartPass(t, w); report.Finished != 1 {
		t.Fatalf("pass after the hour %+v, want the original deleted", report)
	}
	if keys := d.videoKeys(v.Key); len(keys) != 1 || keys[0] != got.Key || d.state(t, v.ID) != "done" {
		t.Fatalf("objects %v, state %q", keys, d.state(t, v.ID))
	}
	if report := faststartPass(t, w); report != (media.FaststartReport{}) {
		t.Fatalf("a pass with nothing to do %+v", report)
	}
}

// A video whose moov comes first already is not_needed, and one the
// rewrite cannot move safely (here a compressed moov) is failed: both stay
// where they are, served as they are, and are not looked at again. The
// refusal is reported with why, by the Media's id.
func TestPostgresFaststartLeavesWhatItNeedNotOrCannotMove(t *testing.T) {
	d := newFaststartDatabase(t)
	ctx := context.Background()
	file, _ := mp4test.TwoTracks().Build()
	layout, err := faststart.Plan(ctx, mp4test.Memory(file), int64(len(file)), faststart.DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	moovFirst, err := layout.Read(ctx, mp4test.Memory(file), 0, layout.Size)
	if err != nil {
		t.Fatal(err)
	}
	compressed := append(append(mp4test.Ftyp(), mp4test.Box("mdat", mp4test.Chunk("v0", 40))...), mp4test.Box("moov", mp4test.Box("cmov", make([]byte, 12)))...)
	fast, broken := d.video(t, moovFirst), d.video(t, compressed)
	w := d.worker(t, d.r2, d.clock)

	var refusals []*media.FaststartRefusal
	report, err := w.Pass(ctx, func(err error) {
		var refusal *media.FaststartRefusal
		if errors.As(err, &refusal) {
			refusals = append(refusals, refusal)
		}
		t.Logf("faststart: %v", err)
	})
	if err != nil || report.NotNeeded != 1 || report.Refused != 1 || report.Rewritten != 0 {
		t.Fatalf("pass %+v, err %v", report, err)
	}
	if len(refusals) != 1 || refusals[0].ID != broken.ID || !errors.Is(refusals[0], faststart.ErrInvalid) {
		t.Fatalf("refusals reported %v", refusals)
	}
	for _, c := range []struct {
		item media.Media
		want string
	}{{fast, "not_needed"}, {broken, "failed"}} {
		item, want := c.item, c.want
		if got := d.get(t, item.ID); got.Key != item.Key || d.state(t, item.ID) != want {
			t.Fatalf("Media at %s in state %q, want %s at %s", got.Key, d.state(t, item.ID), want, item.Key)
		}
		if keys := d.videoKeys(item.Key); len(keys) != 1 || keys[0] != item.Key {
			t.Fatalf("objects %v", keys)
		}
	}
	d.now = d.now.Add(48 * time.Hour)
	if report := faststartPass(t, w); report != (media.FaststartReport{}) {
		t.Fatalf("they were looked at again: %+v", report)
	}
}

// A video larger than a part is never downloaded: the faststart copy is a
// multipart upload of equal parts (as R2 wants them) where core writes only
// the parts that hold the rewritten moov or border it, and storage copies
// every other part from the original's bytes (UploadPartCopy). A smaller
// one is written whole (one PutObject).
func TestPostgresFaststartCopiesALargeVideoInStorage(t *testing.T) {
	d := newFaststartDatabase(t)
	m := mp4test.Movie{Tracks: [][]int{{}, {}}}
	for i := 0; i < 13; i++ {
		m.Chunks = append(m.Chunks, mp4test.Chunk(fmt.Sprintf("c%d", i), 1<<20))
		m.Tracks[i%2] = append(m.Tracks[i%2], i)
	}
	file, _ := m.Build()
	v := d.video(t, file)
	puts := d.fake.Count("PutObject")
	w := d.worker(t, d.r2, d.clock)

	if report := faststartPass(t, w); report.Rewritten != 1 {
		t.Fatalf("pass %+v", report)
	}
	stored, ok := d.fake.Object("media", d.movedKey(t, v))
	if !ok || stored.ContentType != "video/mp4" {
		t.Fatalf("no faststart copy served to play (found %v)", ok)
	}
	mp4test.AssertSamePlayback(t, m, stored.Data)
	copies, written := d.fake.Count("UploadPartCopy"), d.fake.Count("UploadPart")
	if copies != 2 || written != 1 || d.fake.Count("PutObject") != puts || len(d.fake.OpenUploads("media")) != 0 {
		t.Fatalf("%d parts copied, %d written, %d puts, open %v: want the media data copied by storage", copies, written, d.fake.Count("PutObject")-puts, d.fake.OpenUploads("media"))
	}
	if served := d.fake.Served(); served > int64(len(file))/2 {
		t.Fatalf("core downloaded %d of the video's %d bytes", served, len(file))
	}
}

// hookedFaststartStorage is the bucket with hooks around the first write of
// a faststart copy (a PutObject), as a slow or racing storage would be.
type hookedFaststartStorage struct {
	media.FaststartStorage
	once              sync.Once
	beforePut, onPut  func()
	failOriginalReads bool
}

func (h *hookedFaststartStorage) Put(ctx context.Context, key string, data []byte, meta media.BlobMetadata) error {
	var before, after func()
	h.once.Do(func() { before, after = h.beforePut, h.onPut })
	if before != nil {
		before()
	}
	err := h.FaststartStorage.Put(ctx, key, data, meta)
	if after != nil {
		after()
	}
	return err
}

// expired makes the video's pending expiry pass: the expiry purge takes it
// once nothing holds it.
func (d *faststartDatabase) expired(t *testing.T, id uuid.UUID) {
	t.Helper()
	if _, err := d.pool.Exec(context.Background(), `UPDATE media SET expires_at = $2 WHERE id = $1`, id, d.now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
}

func (d *faststartDatabase) purgeExpired(t *testing.T, at time.Time) media.ExpiryReport {
	t.Helper()
	report, err := media.PurgeExpired(context.Background(), d.store, d.r2, at, func(err error) { t.Logf("purge: %v", err) })
	if err != nil {
		t.Fatal(err)
	}
	return report
}

// Two core replicas run side by side on every rolling deploy. A video is
// claimed before any storage work: while one worker writes its faststart
// copy, the other leaves it alone. It is rewritten once.
func TestPostgresTwoWorkersRewriteAVideoOnce(t *testing.T) {
	d := newFaststartDatabase(t)
	file, _ := mp4test.TwoTracks().Build()
	v := d.video(t, file)
	var second media.FaststartReport
	a := d.worker(t, &hookedFaststartStorage{FaststartStorage: d.r2, onPut: func() {
		second = faststartPass(t, d.worker(t, d.r2, d.clock))
	}}, d.clock)
	if report := faststartPass(t, a); report.Rewritten != 1 {
		t.Fatalf("first worker %+v", report)
	}
	if second.Claimed != 0 {
		t.Fatalf("the second worker took a claimed video: %+v", second)
	}
	if got := d.get(t, v.ID); !copyOf(v.Key, got.Key) {
		t.Fatalf("Media at %s", got.Key)
	}
}

// A worker whose lease ran out lost its claim: another worker rewrote the
// video meanwhile, to the same key, and moved the Media there. The first
// moves nothing and must not delete what the Media now points to.
func TestPostgresAFaststartWorkerThatLostItsClaimDeletesNothing(t *testing.T) {
	d := newFaststartDatabase(t)
	m := mp4test.TwoTracks()
	file, _ := m.Build()
	v := d.video(t, file)
	var second media.FaststartReport
	a := d.worker(t, &hookedFaststartStorage{FaststartStorage: d.r2, onPut: func() {
		second = faststartPass(t, d.worker(t, d.r2, func() time.Time { return d.now.Add(24 * time.Hour) }))
	}}, d.clock)
	if report := faststartPass(t, a); report.Rewritten != 0 {
		t.Fatalf("the worker that lost its claim counted %+v", report)
	}
	if second.Rewritten != 1 {
		t.Fatalf("second worker %+v", second)
	}
	got := d.get(t, v.ID)
	stored, ok := d.fake.Object("media", got.Key)
	if !copyOf(v.Key, got.Key) || !ok {
		t.Fatalf("Media at %s, its object present %v", got.Key, ok)
	}
	mp4test.AssertSamePlayback(t, m, stored.Data)
}

// The archive and expiry purges wait while a rewrite's claim is live, so
// its copy can never land after them. Once the Media has moved, a purge
// deletes both keys, whichever the Media points at: the faststart copy and
// the original still in its hour.
func TestPostgresAPurgeWaitsForARewriteAndTakesBothKeys(t *testing.T) {
	d := newFaststartDatabase(t)
	file, _ := mp4test.TwoTracks().Build()
	v := d.video(t, file)
	d.expired(t, v.ID)
	var during media.ExpiryReport
	w := d.worker(t, &hookedFaststartStorage{FaststartStorage: d.r2, onPut: func() {
		during = d.purgeExpired(t, d.now)
	}}, d.clock)
	if report := faststartPass(t, w); report.Rewritten != 1 {
		t.Fatalf("pass %+v", report)
	}
	if during.Purged != 0 || during.Kept != 1 {
		t.Fatalf("purge during the rewrite %+v, want it to wait", during)
	}
	if keys := d.videoKeys(v.Key); len(keys) != 2 {
		t.Fatalf("objects %v, want the original and its copy", keys)
	}
	if report := d.purgeExpired(t, d.now); report.Purged != 1 {
		t.Fatalf("purge after the rewrite %+v", report)
	}
	if keys := d.videoKeys(v.Key); len(keys) != 0 {
		t.Fatalf("the purge left %v", keys)
	}
}

// A purge that did not wait (the rewrite's lease ran out) never leaves the
// faststart copy behind: not when the copy lands after the purge deleted
// both keys, nor when the purge claimed the Media and the copy was checked
// before the Media could move to it.
func TestPostgresAPurgeRacingARewriteLeavesNoCopy(t *testing.T) {
	late := func(d *faststartDatabase) time.Time { return d.now.Add(24 * time.Hour) }
	t.Run("the copy lands after the purge", func(t *testing.T) {
		d := newFaststartDatabase(t)
		file, _ := mp4test.TwoTracks().Build()
		v := d.video(t, file)
		d.expired(t, v.ID)
		w := d.worker(t, &hookedFaststartStorage{FaststartStorage: d.r2, beforePut: func() {
			if report := d.purgeExpired(t, late(d)); report.Purged != 1 {
				t.Errorf("purge %+v", report)
			}
		}}, d.clock)
		faststartPass(t, w)
		if keys := d.videoKeys(v.Key); len(keys) != 0 {
			t.Fatalf("left %v", keys)
		}
		if got := d.get(t, v.ID); got.BlobPurgedAt == nil || got.Key != v.Key {
			t.Fatalf("Media at %s, purged %v", got.Key, got.BlobPurgedAt)
		}
	})
	t.Run("the purge claimed the Media first", func(t *testing.T) {
		d := newFaststartDatabase(t)
		file, _ := mp4test.TwoTracks().Build()
		v := d.video(t, file)
		d.expired(t, v.ID)
		w := d.worker(t, &hookedFaststartStorage{FaststartStorage: d.r2, onPut: func() {
			// The purge claims the Media and fails to delete: its claim stays.
			_, err := d.store.PurgeExpiredBlobIfUnattached(context.Background(), v.ID, late(d), func(string) error { return errors.New("r2: 503") })
			if err == nil {
				t.Error("the purge's delete did not fail")
			}
		}}, d.clock)
		if report := faststartPass(t, w); report.Rewritten != 0 {
			t.Fatalf("pass %+v", report)
		}
		if keys := d.videoKeys(v.Key); len(keys) != 1 || keys[0] != v.Key {
			t.Fatalf("objects %v, want only the original the purge holds", keys)
		}
		if report := d.purgeExpired(t, late(d)); report.Purged != 1 {
			t.Fatalf("the purge finishing %+v", report)
		}
		if keys := d.videoKeys(v.Key); len(keys) != 0 {
			t.Fatalf("left %v", keys)
		}
	})
}

func (d *faststartDatabase) attempts(t *testing.T, id uuid.UUID) (int, *time.Time) {
	t.Helper()
	var n int
	var retry *time.Time
	if err := d.pool.QueryRow(context.Background(), `SELECT video_faststart_attempts, video_faststart_retry_at FROM media WHERE id = $1`, id).Scan(&n, &retry); err != nil {
		t.Fatal(err)
	}
	return n, retry
}

// A step storage fails is tried again later, a minute after the first
// failure, doubling after each: the video stays as it is meanwhile, and a
// multipart copy cut short leaves no parts behind. A rewrite that keeps
// failing is given up (failed, served as it is) after its twelfth attempt;
// deleting a moved video's original never is.
func TestPostgresFaststartTriesAgainAfterAStorageFailure(t *testing.T) {
	d := newFaststartDatabase(t)
	m := mp4test.Movie{Tracks: [][]int{{}}}
	for i := 0; i < 11; i++ {
		m.Chunks = append(m.Chunks, mp4test.Chunk(fmt.Sprintf("c%d", i), 1<<20))
		m.Tracks[0] = append(m.Tracks[0], i)
	}
	file, _ := m.Build()
	v := d.video(t, file)
	w := d.worker(t, d.r2, d.clock)

	// 403: the SDK retries a 5xx by itself.
	d.fake.Fail("UploadPartCopy", 403, "AccessDenied")
	if report := faststartPass(t, w); report.Retried != 1 || report.Rewritten != 0 {
		t.Fatalf("pass %+v", report)
	}
	attempts, retry := d.attempts(t, v.ID)
	if got := d.get(t, v.ID); got.Key != v.Key || d.state(t, v.ID) != "" || attempts != 1 || retry == nil || !retry.Equal(d.now.Add(time.Minute).Truncate(time.Microsecond)) {
		t.Fatalf("Media at %s, state %q, attempts %d, retry at %v", got.Key, d.state(t, v.ID), attempts, retry)
	}
	if keys, open := d.videoKeys(v.Key), d.fake.OpenUploads("media"); len(keys) != 1 || len(open) != 0 {
		t.Fatalf("left %v, open uploads %v", keys, open)
	}
	if report := faststartPass(t, w); report.Claimed != 0 {
		t.Fatalf("tried again before its time: %+v", report)
	}
	d.now = d.now.Add(time.Minute)
	if report := faststartPass(t, w); report.Rewritten != 1 {
		t.Fatalf("pass after a minute %+v", report)
	}
	if attempts, retry := d.attempts(t, v.ID); attempts != 0 || retry == nil || !retry.Equal(d.now.Add(media.FaststartOriginalGrace).Truncate(time.Microsecond)) {
		t.Fatalf("after the move: attempts %d, retry at %v", attempts, retry)
	}

	// The original's delete keeps failing: tried again, never given up.
	d.now = d.now.Add(media.FaststartOriginalGrace)
	if _, err := d.pool.Exec(context.Background(), `UPDATE media SET video_faststart_attempts = 20 WHERE id = $1`, v.ID); err != nil {
		t.Fatal(err)
	}
	d.fake.Fail("DeleteObject", 403, "AccessDenied")
	if report := faststartPass(t, w); report.Retried != 1 || report.Abandoned != 0 || d.state(t, v.ID) != "moved" {
		t.Fatalf("pass %+v, state %q", report, d.state(t, v.ID))
	}

	stuck := d.video(t, file)
	if _, err := d.pool.Exec(context.Background(), `UPDATE media SET video_faststart_attempts = 11 WHERE id = $1`, stuck.ID); err != nil {
		t.Fatal(err)
	}
	d.fake.Fail("HeadObject", 403, "AccessDenied")
	if report := faststartPass(t, w); report.Abandoned != 1 || d.state(t, stuck.ID) != "failed" {
		t.Fatalf("pass %+v, state %q", report, d.state(t, stuck.ID))
	}
	if got := d.get(t, stuck.ID); got.Key != stuck.Key {
		t.Fatalf("the video given up moved to %s", got.Key)
	}
}

// failingPutStorage runs hook, then fails the write: a worker's write that
// fails after another worker moved the Media.
type failingPutStorage struct {
	media.FaststartStorage
	hook func()
}

func (f *failingPutStorage) Put(context.Context, string, []byte, media.BlobMetadata) error {
	f.hook()
	return errors.New("r2: 503 after a stall")
}

// A worker whose lease ran out (it stalled, or its clock is behind the
// other replica's) and whose write then fails must not delete the copy
// another worker has moved the Media to, and an hour later the original
// goes only once that copy is there: the Media never points at nothing.
func TestPostgresALateWorkerNeverDeletesTheCopyTheMediaMovedTo(t *testing.T) {
	d := newFaststartDatabase(t)
	m := mp4test.TwoTracks()
	file, _ := m.Build()
	v := d.video(t, file)
	late := d.now.Add(24 * time.Hour)
	var second media.FaststartReport
	a := d.worker(t, &failingPutStorage{FaststartStorage: d.r2, hook: func() {
		second = faststartPass(t, d.worker(t, d.r2, func() time.Time { return late }))
	}}, d.clock)
	if report := faststartPass(t, a); report.Rewritten != 0 || second.Rewritten != 1 {
		t.Fatalf("late worker %+v, second %+v", report, second)
	}
	if got := d.get(t, v.ID); !copyOf(v.Key, got.Key) {
		t.Fatalf("Media at %s", got.Key)
	}
	if _, ok := d.fake.Object("media", d.get(t, v.ID).Key); !ok {
		t.Fatal("the late worker deleted the copy the Media points at")
	}

	d.now = late.Add(media.FaststartOriginalGrace)
	if report := faststartPass(t, d.worker(t, d.r2, d.clock)); report.Finished != 1 {
		t.Fatalf("the hour after %+v", report)
	}
	got := d.get(t, v.ID)
	stored, ok := d.fake.Object("media", got.Key)
	if !ok || d.state(t, v.ID) != "done" {
		t.Fatalf("Media %s at %s, its object present %v", d.state(t, v.ID), got.Key, ok)
	}
	mp4test.AssertSamePlayback(t, m, stored.Data)
	if keys := d.videoKeys(v.Key); len(keys) != 1 {
		t.Fatalf("objects %v", keys)
	}
}

// The hour after a move, the original goes only once the faststart copy is
// there, whole. A copy that is gone (deleted by a worker whose claim ran
// out) or not whole moves the Media back to its original, which the next
// pass rewrites again. With the original gone too, the video is failed.
func TestPostgresTheHourChecksTheCopyBeforeTheOriginalGoes(t *testing.T) {
	ctx := context.Background()
	for name, damage := range map[string]func(d *faststartDatabase, copyKey string){
		"the copy is gone": func(d *faststartDatabase, copyKey string) {
			if err := d.r2.Delete(ctx, copyKey); err != nil {
				t.Fatal(err)
			}
		},
		"the copy is not whole": func(d *faststartDatabase, copyKey string) {
			if err := d.r2.Put(ctx, copyKey, []byte("cut short"), media.BlobMetadata{ContentType: "video/mp4"}); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			d := newFaststartDatabase(t)
			m := mp4test.TwoTracks()
			file, _ := m.Build()
			v := d.video(t, file)
			w := d.worker(t, d.r2, d.clock)
			if report := faststartPass(t, w); report.Rewritten != 1 || d.state(t, v.ID) != "moved" {
				t.Fatalf("pass %+v, state %q", report, d.state(t, v.ID))
			}
			damage(d, d.movedKey(t, v))
			d.now = d.now.Add(media.FaststartOriginalGrace)
			if report := faststartPass(t, w); report.MovedBack != 1 || report.Finished != 0 {
				t.Fatalf("the hour after %+v", report)
			}
			if got := d.get(t, v.ID); got.Key != v.Key || got.Size != int64(len(file)) || d.state(t, v.ID) != "" {
				t.Fatalf("Media at %s of %d bytes in state %q, want back at %s", got.Key, got.Size, d.state(t, v.ID), v.Key)
			}
			if _, ok := d.fake.Object("media", v.Key); !ok {
				t.Fatal("the original went without its copy")
			}
			if report := faststartPass(t, w); report.Rewritten != 1 {
				t.Fatalf("the rewrite again %+v", report)
			}
			stored, _ := d.fake.Object("media", d.movedKey(t, v))
			mp4test.AssertSamePlayback(t, m, stored.Data)
		})
	}

	t.Run("both are gone", func(t *testing.T) {
		d := newFaststartDatabase(t)
		file, _ := mp4test.TwoTracks().Build()
		v := d.video(t, file)
		w := d.worker(t, d.r2, d.clock)
		faststartPass(t, w)
		for _, key := range []string{v.Key, d.movedKey(t, v)} {
			if err := d.r2.Delete(ctx, key); err != nil {
				t.Fatal(err)
			}
		}
		d.now = d.now.Add(media.FaststartOriginalGrace)
		if report := faststartPass(t, w); report.Refused != 1 || d.state(t, v.ID) != "failed" {
			t.Fatalf("pass %+v, state %q", report, d.state(t, v.ID))
		}
	})
}

// panickingStorage panics reading one object: a bug in the rewrite.
type panickingStorage struct {
	media.FaststartStorage
	key string
}

func (p *panickingStorage) OpenRange(ctx context.Context, key string, off, n int64) (io.ReadCloser, error) {
	if key == p.key {
		panic("a parser bug holding " + key)
	}
	return p.FaststartStorage.OpenRange(ctx, key, off, n)
}

// A video whose rewrite panics is failed and served as it is, its attempt
// counted, and only its id logged; the pass goes on to the next video.
func TestPostgresAFaststartPanicFailsOnlyThatVideo(t *testing.T) {
	d := newFaststartDatabase(t)
	file, _ := mp4test.TwoTracks().Build()
	broken, fine := d.video(t, file), d.video(t, file)
	w := d.worker(t, &panickingStorage{FaststartStorage: d.r2, key: broken.Key}, d.clock)
	var logged []error
	report, err := w.Pass(context.Background(), func(err error) { logged = append(logged, err) })
	if err != nil || report.Panicked != 1 || report.Rewritten != 1 {
		t.Fatalf("pass %+v, err %v", report, err)
	}
	if attempts, _ := d.attempts(t, broken.ID); d.state(t, broken.ID) != "failed" || attempts != 1 || d.get(t, broken.ID).Key != broken.Key {
		t.Fatalf("the video that panicked: state %q, attempts %d", d.state(t, broken.ID), attempts)
	}
	if keys := d.videoKeys(broken.Key); len(keys) != 1 || !copyOf(fine.Key, d.get(t, fine.ID).Key) {
		t.Fatalf("objects %v", keys)
	}
	var failure *media.FaststartError
	if len(logged) != 1 || !errors.As(logged[0], &failure) || failure.ID != broken.ID || strings.Contains(logged[0].Error(), "parser bug") {
		t.Fatalf("logged %v, want the video's id only", logged)
	}
}

// A late worker whose write fails while another worker has written its
// copy but not yet moved the Media there deletes nothing: its claim is
// lost, so the key is the other worker's. That worker checks its copy and
// moves the Media.
func TestPostgresALateWorkerNeverDeletesAnotherWorkersCopy(t *testing.T) {
	d := newFaststartDatabase(t)
	m := mp4test.TwoTracks()
	file, _ := m.Build()
	v := d.video(t, file)
	aWriting, bLanded, aDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	a := d.worker(t, &failingPutStorage{FaststartStorage: d.r2, hook: func() {
		close(aWriting)
		<-bLanded
	}}, d.clock)
	var aReport media.FaststartReport
	var aErr error
	go func() {
		defer close(aDone)
		aReport, aErr = a.Pass(context.Background(), func(err error) { t.Logf("late worker: %v", err) })
	}()
	<-aWriting
	b := d.worker(t, &hookedFaststartStorage{FaststartStorage: d.r2, onPut: func() {
		close(bLanded)
		<-aDone
	}}, func() time.Time { return d.now.Add(24 * time.Hour) })
	bReport := faststartPass(t, b)
	if aErr != nil || aReport.Rewritten != 0 || bReport.Rewritten != 1 {
		t.Fatalf("late worker %+v (%v), other %+v", aReport, aErr, bReport)
	}
	got := d.get(t, v.ID)
	stored, ok := d.fake.Object("media", got.Key)
	if !copyOf(v.Key, got.Key) || !ok {
		t.Fatalf("Media at %s, its object present %v", got.Key, ok)
	}
	mp4test.AssertSamePlayback(t, m, stored.Data)
}

// isCopyKey reports whether key is a video's faststart copy.
func isCopyKey(key string) bool { return strings.Contains(key, ".fs.") }

// stalledCleanup fails a worker's write of its copy (after onPut), then
// stalls its cleanup: the first delete of a copy key after the failed
// write runs stall first, as a worker that pauses between deciding to
// delete and deleting.
type stalledCleanup struct {
	media.FaststartStorage
	mu              sync.Mutex
	failed, stalled bool
	onPut, stall    func()
	putKey          string
}

func (s *stalledCleanup) Put(_ context.Context, key string, _ []byte, _ media.BlobMetadata) error {
	if s.onPut != nil {
		s.onPut()
	}
	s.mu.Lock()
	s.failed, s.putKey = true, key
	s.mu.Unlock()
	return errors.New("r2: 503")
}

func (s *stalledCleanup) Delete(ctx context.Context, key string) error {
	s.mu.Lock()
	run := s.failed && !s.stalled && isCopyKey(key) && s.stall != nil
	if run {
		s.stalled = true
	}
	s.mu.Unlock()
	if run {
		s.stall()
	}
	return s.FaststartStorage.Delete(ctx, key)
}

// assertServed checks the Media points at an object that is there, in the
// state given.
func (d *faststartDatabase) assertServed(t *testing.T, id uuid.UUID, state string) media.Media {
	t.Helper()
	got := d.get(t, id)
	if _, ok := d.fake.Object("media", got.Key); !ok || d.state(t, id) != state {
		t.Fatalf("Media %q at %s, its object present %v; keys %v", d.state(t, id), got.Key, ok, d.videoKeys(got.Key))
	}
	return got
}

// Probe 1: a worker whose write fails decides to delete its copy, then
// stalls past its lease; another worker rewrites the video and moves the
// Media meanwhile. The late delete lands, and must not take the copy the
// Media points at: each claim writes and deletes only its own key.
func TestPostgresAStalledCleanupNeverDeletesTheMovedCopy(t *testing.T) {
	d := newFaststartDatabase(t)
	file, _ := mp4test.TwoTracks().Build()
	v := d.video(t, file)
	late := d.now.Add(24 * time.Hour)
	var second media.FaststartReport
	a := d.worker(t, &stalledCleanup{FaststartStorage: d.r2, stall: func() {
		second = faststartPass(t, d.worker(t, d.r2, func() time.Time { return late }))
	}}, d.clock)
	if report := faststartPass(t, a); report.Rewritten != 0 || second.Rewritten != 1 {
		t.Fatalf("stalled worker %+v, other %+v", report, second)
	}
	d.assertServed(t, v.ID, "moved")
}

// Probe 1b: no stall at all. The first worker spent its step's whole time
// before its write failed; the other's clock runs two minutes ahead (the
// skew the docs allow), so it claims the video while the first one's
// cleanup is under way. The Media still never points at nothing.
func TestPostgresACleanupWithinTheClockSkewNeverDeletesTheMovedCopy(t *testing.T) {
	d := newFaststartDatabase(t)
	file, _ := mp4test.TwoTracks().Build()
	v := d.video(t, file)
	start := d.now
	var mu sync.Mutex
	aNow := start
	aClock := func() time.Time { mu.Lock(); defer mu.Unlock(); return aNow }
	work := 5*time.Minute + time.Duration(int64(len(file))>>20)*time.Second
	var second media.FaststartReport
	a := d.worker(t, &stalledCleanup{FaststartStorage: d.r2,
		onPut: func() { mu.Lock(); aNow = start.Add(work); mu.Unlock() },
		stall: func() {
			second = faststartPass(t, d.worker(t, d.r2, func() time.Time { return aClock().Add(2 * time.Minute) }))
		}}, aClock)
	if report := faststartPass(t, a); report.Rewritten != 0 || second.Rewritten != 1 {
		t.Fatalf("first worker %+v, second %+v", report, second)
	}
	d.assertServed(t, v.ID, "moved")
}

// afterHead runs hook once, right after the HEAD of key: a stale delete
// landing between the hour's check of the copy and its delete of the
// original.
type afterHead struct {
	media.FaststartStorage
	mu   sync.Mutex
	key  string
	hook func()
}

func (a *afterHead) Size(ctx context.Context, key string) (int64, error) {
	n, err := a.FaststartStorage.Size(ctx, key)
	a.mu.Lock()
	hook := a.hook
	if key != a.key {
		hook = nil
	} else {
		a.hook = nil
	}
	a.mu.Unlock()
	if hook != nil {
		hook()
	}
	return n, err
}

// Probe 2: the hour checks the copy the Media points at, then deletes the
// original. A stale worker's delete (of the copy it tried to write) landing
// between the two must not leave the Media with neither: the stale worker's
// key is its own, never the one the Media points at.
func TestPostgresAStaleDeleteDuringTheHourLeavesTheMovedCopy(t *testing.T) {
	d := newFaststartDatabase(t)
	m := mp4test.TwoTracks()
	file, _ := m.Build()
	v := d.video(t, file)
	late := d.now.Add(24 * time.Hour)
	stale := &stalledCleanup{FaststartStorage: d.r2, onPut: func() {
		faststartPass(t, d.worker(t, d.r2, func() time.Time { return late }))
	}}
	faststartPass(t, d.worker(t, stale, d.clock))
	moved := d.assertServed(t, v.ID, "moved")

	d.now = late.Add(media.FaststartOriginalGrace)
	hour := d.worker(t, &afterHead{FaststartStorage: d.r2, key: moved.Key, hook: func() {
		if err := d.r2.Delete(context.Background(), stale.putKey); err != nil {
			t.Error(err)
		}
	}}, d.clock)
	if report := faststartPass(t, hour); report.Finished != 1 {
		t.Fatalf("the hour %+v", report)
	}
	got := d.assertServed(t, v.ID, "done")
	stored, _ := d.fake.Object("media", got.Key)
	mp4test.AssertSamePlayback(t, m, stored.Data)
	if keys := d.videoKeys(v.Key); len(keys) != 1 {
		t.Fatalf("objects %v, want only the copy", keys)
	}
}

// deleteBeforeWrite stalls the first delete of a copy key a worker makes
// before its first write, and counts such deletes.
type deleteBeforeWrite struct {
	media.FaststartStorage
	mu           sync.Mutex
	wrote, fired bool
	deletes      int
	failPut      bool
	stall        func()
}

func (s *deleteBeforeWrite) Put(ctx context.Context, key string, data []byte, meta media.BlobMetadata) error {
	s.mu.Lock()
	s.wrote = true
	s.mu.Unlock()
	if s.failPut {
		return errors.New("r2: 503")
	}
	return s.FaststartStorage.Put(ctx, key, data, meta)
}

func (s *deleteBeforeWrite) Delete(ctx context.Context, key string) error {
	s.mu.Lock()
	run := false
	if !s.wrote && isCopyKey(key) {
		s.deletes++
		run, s.fired = !s.fired, true
	}
	s.mu.Unlock()
	if run {
		s.stall()
	}
	return s.FaststartStorage.Delete(ctx, key)
}

// Probe 5: a worker deletes nothing before it writes its copy, so no
// delete of its can land on a copy another worker moved the Media to
// meanwhile, whether its own write then fails or not.
func TestPostgresAWorkerDeletesNothingBeforeItWrites(t *testing.T) {
	for _, failPut := range []bool{false, true} {
		t.Run(fmt.Sprintf("its write fails: %v", failPut), func(t *testing.T) {
			d := newFaststartDatabase(t)
			file, _ := mp4test.TwoTracks().Build()
			v := d.video(t, file)
			late := d.now.Add(24 * time.Hour)
			st := &deleteBeforeWrite{FaststartStorage: d.r2, failPut: failPut, stall: func() {
				faststartPass(t, d.worker(t, d.r2, func() time.Time { return late }))
			}}
			faststartPass(t, d.worker(t, st, d.clock))
			if st.deletes != 0 {
				t.Errorf("%d deletes of a copy key before the write", st.deletes)
			}
			// A write that fails leaves the video at its original, waiting
			// to be tried again.
			want := "moved"
			if failPut {
				want = ""
			}
			d.assertServed(t, v.ID, want)
		})
	}
}

// strayCopy stores a faststart copy of v as a claim that crashed between
// its write and its cleanup leaves it: at its own key, pointed at by
// nothing.
func (d *faststartDatabase) strayCopy(t *testing.T, v media.Media) string {
	t.Helper()
	key, ok := media.FaststartCopyKey(v.Key, uuid.New())
	if !ok {
		t.Fatalf("no copy key for %s", v.Key)
	}
	if err := d.r2.Put(context.Background(), key, []byte("a copy a crash left"), media.BlobMetadata{ContentType: "video/mp4"}); err != nil {
		t.Fatal(err)
	}
	return key
}

// The hour after a move takes every other copy of the video with its
// original (listed by their prefix): the ones earlier claims could not
// delete. Another video's keys are not its.
func TestPostgresTheHourSweepsTheStrayCopies(t *testing.T) {
	d := newFaststartDatabase(t)
	file, _ := mp4test.TwoTracks().Build()
	v, other := d.video(t, file), d.video(t, file)
	d.strayCopy(t, v)
	w := d.worker(t, d.r2, d.clock)
	faststartPass(t, w)
	d.strayCopy(t, v)
	moved := d.movedKey(t, v)
	if keys := d.videoKeys(v.Key); len(keys) != 4 {
		t.Fatalf("objects %v, want the original, its copy and two strays", keys)
	}
	d.now = d.now.Add(media.FaststartOriginalGrace)
	if report := faststartPass(t, w); report.Finished != 2 {
		t.Fatalf("the hour %+v", report)
	}
	if keys := d.videoKeys(v.Key); !slices.Equal(keys, []string{moved}) || d.state(t, v.ID) != "done" {
		t.Fatalf("objects %v in state %q, want only %s", keys, d.state(t, v.ID), moved)
	}
	if keys := d.videoKeys(other.Key); len(keys) != 1 || !copyOf(other.Key, keys[0]) {
		t.Fatalf("the other video's objects %v", keys)
	}
}

// A rewrite given up on takes its video's stray copies with it: the video
// is served as it is, from its original.
func TestPostgresAFailedRewriteSweepsItsStrayCopies(t *testing.T) {
	d := newFaststartDatabase(t)
	file, _ := mp4test.TwoTracks().Build()
	v := d.video(t, file)
	d.strayCopy(t, v)
	if _, err := d.pool.Exec(context.Background(), `UPDATE media SET video_faststart_attempts = 11 WHERE id = $1`, v.ID); err != nil {
		t.Fatal(err)
	}
	d.fake.Fail("HeadObject", 403, "AccessDenied")
	if report := faststartPass(t, d.worker(t, d.r2, d.clock)); report.Abandoned != 1 || d.state(t, v.ID) != "failed" {
		t.Fatalf("pass %+v, state %q", report, d.state(t, v.ID))
	}
	if keys := d.videoKeys(v.Key); !slices.Equal(keys, []string{v.Key}) {
		t.Fatalf("objects %v, want only the original", keys)
	}
}

// Every purge of a video takes all its keys: the original in its hour, the
// copy the Media points at, and any stray copy, listed by their prefix.
func TestPostgresAPurgeTakesEveryCopyOfAVideo(t *testing.T) {
	d := newFaststartDatabase(t)
	file, _ := mp4test.TwoTracks().Build()
	v := d.video(t, file)
	faststartPass(t, d.worker(t, d.r2, d.clock))
	d.strayCopy(t, v)
	d.expired(t, v.ID)
	if keys := d.videoKeys(v.Key); len(keys) != 3 {
		t.Fatalf("objects %v", keys)
	}
	if report := d.purgeExpired(t, d.now.Add(24*time.Hour)); report.Purged != 1 {
		t.Fatalf("purge %+v", report)
	}
	if keys := d.videoKeys(v.Key); len(keys) != 0 {
		t.Fatalf("the purge left %v", keys)
	}
	if d.state(t, v.ID) != "moved" {
		t.Fatalf("state %q", d.state(t, v.ID))
	}
}

// A video whose rewrite crashed between writing its copy and deleting it
// leaves a stray copy. Its uploader's account erasure keeps the video as
// club content, stray and all, without their name; the video's purge then
// takes every key, the stray too.
func TestPostgresAnErasedUploadersStrayCopyGoesWithTheVideosPurge(t *testing.T) {
	d := newFaststartDatabase(t)
	ctx := context.Background()
	file, _ := mp4test.TwoTracks().Build()
	v := d.video(t, file)
	stray := d.strayCopy(t, v)
	users := user.NewPostgresStore(d.pool)
	request, err := users.RequestDeletion(ctx, d.uploader, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := users.AnonymizeAccount(ctx, d.uploader, d.now, nil); err != nil {
		t.Fatal(err)
	}
	ids, err := users.MediaForDeletion(ctx, request.ID)
	if err != nil || !slices.Contains(ids, v.ID) {
		t.Fatalf("recorded %v, err %v", ids, err)
	}
	eraser := media.NewImmediateBlobEraser(d.store, media.Buckets{Public: d.r2})
	for _, id := range ids {
		if err := eraser.EnsureErased(ctx, id, d.now); err != nil {
			t.Fatal(err)
		}
	}
	if got := d.get(t, v.ID); got.Name != "" || got.BlobPurgedAt != nil {
		t.Fatalf("the video after the erasure: name %q, purged %v", got.Name, got.BlobPurgedAt)
	}
	if keys := d.videoKeys(v.Key); !slices.Equal(keys, []string{stray, v.Key}) && !slices.Equal(keys, []string{v.Key, stray}) {
		t.Fatalf("objects after the erasure %v", keys)
	}
	d.expired(t, v.ID)
	if report := d.purgeExpired(t, d.now.Add(time.Hour)); report.Purged != 1 {
		t.Fatalf("purge %+v", report)
	}
	if keys := d.videoKeys(v.Key); len(keys) != 0 {
		t.Fatalf("the purge left %v", keys)
	}
}

// sweepHook fails the worker's write, and runs beforeList before its sweep
// lists the video's copies.
type sweepHook struct {
	media.FaststartStorage
	beforeList func()
}

func (s *sweepHook) Put(context.Context, string, []byte, media.BlobMetadata) error {
	return errors.New("r2: 503")
}

func (s *sweepHook) ListKeys(ctx context.Context, prefix string) ([]string, error) {
	if s.beforeList != nil {
		s.beforeList()
	}
	return s.FaststartStorage.ListKeys(ctx, prefix)
}

// A stale worker's sweep (it gives the rewrite up) lists a copy a newer
// claim has written but not yet moved the Media to. The sweep checks its
// claim after listing, finds it gone, and deletes nothing: the newer
// worker checks its copy and moves the Media there.
func TestPostgresAStaleSweepNeverDeletesANewerClaimsCopy(t *testing.T) {
	d := newFaststartDatabase(t)
	m := mp4test.TwoTracks()
	file, _ := m.Build()
	v := d.video(t, file)
	if _, err := d.pool.Exec(context.Background(), `UPDATE media SET video_faststart_attempts = 11 WHERE id = $1`, v.ID); err != nil {
		t.Fatal(err)
	}
	aListing, bWritten, aDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	a := d.worker(t, &sweepHook{FaststartStorage: d.r2, beforeList: func() {
		close(aListing)
		<-bWritten
	}}, d.clock)
	go func() {
		defer close(aDone)
		_, _ = a.Pass(context.Background(), func(err error) { t.Logf("stale worker: %v", err) })
	}()
	<-aListing
	b := d.worker(t, &hookedFaststartStorage{FaststartStorage: d.r2, onPut: func() {
		close(bWritten)
		<-aDone
	}}, func() time.Time { return d.now.Add(24 * time.Hour) })
	if report := faststartPass(t, b); report.Rewritten != 1 {
		t.Fatalf("the newer worker %+v", report)
	}
	stored, _ := d.fake.Object("media", d.assertServed(t, v.ID, "moved").Key)
	mp4test.AssertSamePlayback(t, m, stored.Data)
}

// panicOnHead panics sizing one object: a bug in the step after the hour.
type panicOnHead struct {
	media.FaststartStorage
	key string
}

func (p *panicOnHead) Size(ctx context.Context, key string) (int64, error) {
	if key == p.key {
		panic("a bug")
	}
	return p.FaststartStorage.Size(ctx, key)
}

// A step that panics on a moved video keeps it moved, served from its
// copy: the attempt counts, and the step that drops its original is tried
// again after its wait, so the original does not stay until a purge.
func TestPostgresAPanicAfterTheHourKeepsTheVideoMoved(t *testing.T) {
	d := newFaststartDatabase(t)
	file, _ := mp4test.TwoTracks().Build()
	v := d.video(t, file)
	w := d.worker(t, d.r2, d.clock)
	faststartPass(t, w)
	moved := d.movedKey(t, v)
	d.now = d.now.Add(media.FaststartOriginalGrace)
	if report := faststartPass(t, d.worker(t, &panicOnHead{FaststartStorage: d.r2, key: moved}, d.clock)); report.Panicked != 1 {
		t.Fatalf("pass %+v", report)
	}
	if attempts, retry := d.attempts(t, v.ID); d.state(t, v.ID) != "moved" || attempts != 1 || retry == nil {
		t.Fatalf("after the panic: state %q, attempts %d, retry %v", d.state(t, v.ID), attempts, retry)
	}
	d.now = d.now.Add(time.Minute)
	if report := faststartPass(t, w); report.Finished != 1 {
		t.Fatalf("the step again %+v", report)
	}
	d.assertServed(t, v.ID, "done")
	if keys := d.videoKeys(v.Key); !slices.Equal(keys, []string{moved}) {
		t.Fatalf("objects %v", keys)
	}
}

// Ending a rewrite on a Media purged meanwhile changes nothing but letting
// go of the claim.
func TestPostgresFinishingARewriteOfAPurgedVideoChangesNothing(t *testing.T) {
	d := newFaststartDatabase(t)
	ctx := context.Background()
	file, _ := mp4test.TwoTracks().Build()
	v := d.video(t, file)
	claim, found, err := d.store.ClaimNextFaststart(ctx, d.now, uuid.Nil, func(media.Media) time.Duration { return time.Hour })
	if err != nil || !found || claim.Media.ID != v.ID {
		t.Fatalf("claim %+v found %v err %v", claim, found, err)
	}
	if _, err := d.pool.Exec(ctx, `UPDATE media SET deleted_at = now(), blob_purge_started_at = now(), blob_purged_at = now() WHERE id = $1`, v.ID); err != nil {
		t.Fatal(err)
	}
	if done, err := d.store.FinishFaststart(ctx, claim, media.FaststartDone); err != nil || done {
		t.Fatalf("finish: done %v, err %v", done, err)
	}
	var claimed bool
	if err := d.pool.QueryRow(ctx, `SELECT video_faststart_claim_id IS NOT NULL FROM media WHERE id = $1`, v.ID).Scan(&claimed); err != nil || claimed || d.state(t, v.ID) != "" {
		t.Fatalf("state %q, still claimed %v, err %v", d.state(t, v.ID), claimed, err)
	}
}
