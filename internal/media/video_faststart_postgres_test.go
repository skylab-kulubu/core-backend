package media_test

import (
	"context"
	"errors"
	"fmt"
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
	name := strings.TrimSuffix(strings.TrimSuffix(key, ".mp4"), ".fs")
	var out []string
	for _, k := range d.fake.Keys("media") {
		if strings.HasPrefix(k, name+".") {
			out = append(out, k)
		}
	}
	return out
}

func fsKey(key string) string { return strings.TrimSuffix(key, ".mp4") + ".fs.mp4" }

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
	if got.Key != fsKey(v.Key) || got.Size != int64(len(file)) {
		t.Fatalf("Media at %s of %d bytes, want %s", got.Key, got.Size, fsKey(v.Key))
	}
	stored, ok := d.fake.Object("media", got.Key)
	if !ok || stored.ContentType != "video/mp4" || stored.ContentDisposition != "" {
		t.Fatalf("faststart copy %q %q (found %v), want video/mp4 inline", stored.ContentType, stored.ContentDisposition, ok)
	}
	mp4test.AssertSamePlayback(t, m, stored.Data)
	if _, ok := d.fake.Object("media", v.Key); !ok || d.state(t, v.ID) != "" {
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
	stored, ok := d.fake.Object("media", fsKey(v.Key))
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
	if got := d.get(t, v.ID); got.Key != fsKey(v.Key) {
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
	if got.Key != fsKey(v.Key) || !ok {
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
	if report := faststartPass(t, w); report.Retried != 1 || report.Abandoned != 0 || d.state(t, v.ID) != "" {
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
