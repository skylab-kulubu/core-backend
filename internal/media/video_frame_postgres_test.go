package media_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/media/s3test"
	"github.com/skylab-kulubu/core-backend/internal/mediaframe"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// frameDatabase is core's database, a fake R2 bucket, and a fake frame
// service, with a clock the tests move.
type frameDatabase struct {
	pool     *pgxpool.Pool
	store    *media.PostgresStore
	r2       *media.R2
	fake     *s3test.Server
	uploader uuid.UUID
	now      time.Time
	frames   *fakeFrames
}

func newFrameDatabase(t *testing.T) *frameDatabase {
	t.Helper()
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	uploader := uuid.New()
	if _, _, err := user.NewService(user.NewPostgresStore(pool)).Ensure(ctx, uploader, user.Profile{Email: "frames@example.test"}); err != nil {
		t.Fatal(err)
	}
	r2, fake := multipartR2(t)
	d := &frameDatabase{pool: pool, store: media.NewPostgresStore(pool), r2: r2, fake: fake, uploader: uploader, now: time.Now().UTC()}
	d.frames = newFakeFrames(t)
	return d
}

// fakeFrames is a frame service: it reads the video by the address it is
// given, as the real one does, and answers a 1280x720 JPEG, or what answer
// says.
type fakeFrames struct {
	server *httptest.Server
	mu     sync.Mutex
	asked  []mediaframe.Request
	// read are the videos' bytes the service read, by path.
	read map[string][]byte
	// answer, when set, answers instead.
	answer func(w http.ResponseWriter, req mediaframe.Request) bool
}

func newFakeFrames(t *testing.T) *fakeFrames {
	t.Helper()
	f := &fakeFrames{read: map[string][]byte{}}
	frame := testFrameJPEG(t, 1280, 720)
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req mediaframe.Request
		if r.Method != http.MethodPost || r.URL.Path != "/frame" || json.NewDecoder(r.Body).Decode(&req) != nil {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.asked = append(f.asked, req)
		answer := f.answer
		f.mu.Unlock()
		if answer != nil && answer(w, req) {
			return
		}
		resp, err := http.Get(req.URL)
		if err != nil {
			problem(w, http.StatusBadGateway, mediaframe.CodeUpstream)
			return
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			problem(w, http.StatusBadGateway, mediaframe.CodeUpstream)
			return
		}
		u, _ := url.Parse(req.URL)
		f.mu.Lock()
		f.read[u.Path] = body
		f.mu.Unlock()
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write(frame)
	}))
	t.Cleanup(f.server.Close)
	return f
}

func problem(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"status":%d,"code":%q}`, status, code)
}

func (f *fakeFrames) setAnswer(answer func(w http.ResponseWriter, req mediaframe.Request) bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answer = answer
}

func (f *fakeFrames) requests() []mediaframe.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]mediaframe.Request(nil), f.asked...)
}

func (f *fakeFrames) client(t *testing.T) *mediaframe.Client {
	t.Helper()
	client, err := mediaframe.NewClient(strings.TrimPrefix(f.server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// testFrameJPEG is a w by h JPEG, grey.
func testFrameJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = 120
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, img, nil); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// eventVideo is an Event (of WEBLAB) listing one video, whose faststart is
// settled as faststart says ("" leaves it waiting): the Event's id, and the
// video's Media.
func (d *frameDatabase) eventVideo(t *testing.T, faststart string) (uuid.UUID, media.Media) {
	t.Helper()
	ctx := context.Background()
	eventID := uuid.New()
	if _, err := d.pool.Exec(ctx, `INSERT INTO events (id, name, location, owner_team) VALUES ($1, 'Hack', 'YTÜ', 'WEBLAB')`, eventID); err != nil {
		t.Fatal(err)
	}
	return eventID, d.addVideo(t, eventID, faststart)
}

// addVideo adds a video to the Event's list.
func (d *frameDatabase) addVideo(t *testing.T, eventID uuid.UUID, faststart string) media.Media {
	t.Helper()
	ctx := context.Background()
	data := []byte("video bytes " + uuid.NewString())
	key := "videos/" + uuid.NewString() + ".mp4"
	if err := d.r2.Put(ctx, key, data, media.ServingMetadataFor(media.PurposeVideo, "video/mp4", "açılış.mp4")); err != nil {
		t.Fatal(err)
	}
	expires := d.now.Add(24 * time.Hour)
	video, err := d.store.Create(ctx, media.Media{
		Name: "açılış.mp4", Type: "video/mp4", Kind: media.KindFile, Key: key, Size: int64(len(data)),
		UploadedBy: d.uploader, Purpose: media.PurposeVideo, Status: media.StatusPending, ExpiresAt: &expires,
		Visibility: media.VisibilityPublic, ServingPolicyApplied: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if faststart != "" {
		if _, err := d.pool.Exec(ctx, `UPDATE media SET video_faststart = $2 WHERE id = $1`, video.ID, faststart); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.pool.Exec(ctx, `INSERT INTO event_videos (event_id, media_id, order_index)
		VALUES ($1, $2, COALESCE((SELECT max(order_index) FROM event_videos WHERE event_id = $1), 0) + 1)`, eventID, video.ID); err != nil {
		t.Fatal(err)
	}
	return video
}

func (d *frameDatabase) worker(t *testing.T, frames media.FrameSource) *media.FrameWorker {
	t.Helper()
	w, err := media.NewFrameWorker(media.FrameWorkerConfig{
		Store: d.store, Storage: d.r2, Frames: frames, Now: func() time.Time { return d.now },
	})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func framePass(t *testing.T, w *media.FrameWorker) media.FrameReport {
	t.Helper()
	report, err := w.Pass(context.Background(), func(err error) { t.Logf("frame: %v", err) })
	if err != nil {
		t.Fatal(err)
	}
	return report
}

// videoFrame is where the video's frame is: its frame Media (nil for
// none), state, attempts and next try.
type videoFrame struct {
	frame    *uuid.UUID
	state    string
	attempts int
	retryAt  *time.Time
	claimed  bool
}

func (d *frameDatabase) frameOf(t *testing.T, eventID, videoID uuid.UUID) videoFrame {
	t.Helper()
	var f videoFrame
	if err := d.pool.QueryRow(context.Background(), `SELECT frame_media_id, COALESCE(frame_state, ''), frame_attempts, frame_retry_at, frame_claim_id IS NOT NULL
		FROM event_videos WHERE event_id = $1 AND media_id = $2`, eventID, videoID).
		Scan(&f.frame, &f.state, &f.attempts, &f.retryAt, &f.claimed); err != nil {
		t.Fatal(err)
	}
	return f
}

func (d *frameDatabase) get(t *testing.T, id uuid.UUID) media.Media {
	t.Helper()
	got, err := d.store.GetIncludingDeleted(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// attachedAs is how many Media attachments of the Event hold the Media in
// the role.
func (d *frameDatabase) attachedAs(t *testing.T, mediaID, eventID uuid.UUID, role string) int {
	t.Helper()
	var n int
	if err := d.pool.QueryRow(context.Background(), `SELECT count(*) FROM media_attachments
		WHERE media_id = $1 AND owner_service = 'core' AND owner_type = 'event' AND owner_id = $2 AND role = $3`,
		mediaID, eventID.String(), role).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A video with no uploaded poster, settled by faststart, gets a frame: the
// worker hands the frame service a presigned GET of the video and asks for
// the frame at one second; the JPEG it answers goes through the image
// pipeline (re-encoded, with its card and page sizes) and is stored as a
// public video_frame Media with no uploader and no name, which the video's
// link holds (event_videos.frame_media_id) and the Event attaches in the
// role event_video_frame. A video that has its frame is done.
func TestPostgresFrameWorkerStoresAndAttachesAVideosFrame(t *testing.T) {
	d := newFrameDatabase(t)
	eventID, video := d.eventVideo(t, string(media.FaststartDone))
	w := d.worker(t, d.frames.client(t))

	if report := framePass(t, w); report.Made != 1 || report.Claimed != 1 {
		t.Fatalf("pass %+v", report)
	}
	asked := d.frames.requests()
	if len(asked) != 1 || asked[0].AtMillis != 1000 {
		t.Fatalf("the frame service was asked %+v", asked)
	}
	address, err := url.Parse(asked[0].URL)
	if err != nil || address.Path != "/media/"+video.Key || address.Query().Get("X-Amz-Signature") == "" || address.Query().Get("X-Amz-Expires") == "" {
		t.Fatalf("the address %q is not a presigned GET of the video", asked[0].URL)
	}
	if read := d.frames.read[address.Path]; !bytes.HasPrefix(read, []byte("video bytes ")) {
		t.Fatalf("the frame service read %q by it", read)
	}

	f := d.frameOf(t, eventID, video.ID)
	if f.frame == nil || f.state != "" || f.attempts != 0 || f.claimed {
		t.Fatalf("video's frame %+v", f)
	}
	frame := d.get(t, *f.frame)
	if frame.Purpose != media.PurposeVideoFrame || frame.Type != "image/jpeg" || frame.Kind != media.KindImage ||
		frame.Visibility != media.VisibilityPublic || frame.Status != media.StatusAttached || frame.ExpiresAt != nil {
		t.Fatalf("frame Media %+v", frame)
	}
	if frame.UploadedBy != uuid.Nil || frame.Name != "" {
		t.Fatalf("the frame has an uploader (%s) or a name (%q)", frame.UploadedBy, frame.Name)
	}
	if frame.Width != 1280 || frame.Height != 720 || frame.SizeObjects["card"].Width != 400 || frame.SizeObjects["page"].Width != 1200 {
		t.Fatalf("frame %dx%d, sizes %+v", frame.Width, frame.Height, frame.SizeObjects)
	}
	if !strings.HasPrefix(frame.Key, "images/") {
		t.Fatalf("frame key %q", frame.Key)
	}
	for _, key := range []string{frame.Key, frame.Key + "/card.jpg", frame.Key + "/page.jpg"} {
		if object, ok := d.fake.Object("media", key); !ok || object.ContentType != "image/jpeg" || object.ContentDisposition != "" {
			t.Errorf("object %s: %v %+v", key, ok, object)
		}
	}
	if n := d.attachedAs(t, frame.ID, eventID, "event_video_frame"); n != 1 {
		t.Fatalf("frame attached %d times as the Event's event_video_frame", n)
	}

	if report := framePass(t, w); report.Claimed != 0 || len(d.frames.requests()) != 1 {
		t.Fatalf("a video with its frame was taken again: %+v", report)
	}
}

// The worker waits for a video faststart has settled (moved to its copy,
// done, not needed, or failed and served as it is): before that its key
// moves. It leaves a video with an uploaded poster, a video that cannot be
// served, and an archived Event's.
func TestPostgresFrameWorkerTakesOnlyVideosThatNeedAFrame(t *testing.T) {
	d := newFrameDatabase(t)
	ctx := context.Background()
	waiting, waitingVideo := d.eventVideo(t, "")
	settled := map[string]uuid.UUID{}
	for _, state := range []media.FaststartState{media.FaststartMoved, media.FaststartDone, media.FaststartNotNeeded, media.FaststartFailed} {
		_, video := d.eventVideo(t, string(state))
		settled[string(state)] = video.ID
	}
	postered, posteredVideo := d.eventVideo(t, string(media.FaststartDone))
	poster := d.posterImage(t)
	if _, err := d.pool.Exec(ctx, `UPDATE event_videos SET poster_media_id = $3 WHERE event_id = $1 AND media_id = $2`, postered, posteredVideo.ID, poster); err != nil {
		t.Fatal(err)
	}
	archivedVideoOf, archivedVideo := d.eventVideo(t, string(media.FaststartDone))
	if _, err := d.pool.Exec(ctx, `UPDATE events SET archived_at = now() WHERE id = $1`, archivedVideoOf); err != nil {
		t.Fatal(err)
	}
	gone, goneVideo := d.eventVideo(t, string(media.FaststartDone))
	if _, err := d.pool.Exec(ctx, `UPDATE media SET deleted_at = now() WHERE id = $1`, goneVideo.ID); err != nil {
		t.Fatal(err)
	}

	report := framePass(t, d.worker(t, d.frames.client(t)))
	if report.Made != 4 {
		t.Fatalf("pass %+v, want the four settled videos' frames", report)
	}
	for state, id := range settled {
		var has bool
		if err := d.pool.QueryRow(ctx, `SELECT frame_media_id IS NOT NULL FROM event_videos WHERE media_id = $1`, id).Scan(&has); err != nil || !has {
			t.Errorf("a video %s has no frame (%v)", state, err)
		}
	}
	for name, v := range map[string][2]uuid.UUID{
		"waiting for faststart": {waiting, waitingVideo.ID},
		"with a poster":         {postered, posteredVideo.ID},
		"of an archived Event":  {archivedVideoOf, archivedVideo.ID},
		"archived":              {gone, goneVideo.ID},
	} {
		if f := d.frameOf(t, v[0], v[1]); f.frame != nil || f.attempts != 0 || f.claimed {
			t.Errorf("a video %s: %+v", name, f)
		}
	}

	// Clearing the poster makes the video wait for a frame.
	if _, err := d.pool.Exec(ctx, `UPDATE event_videos SET poster_media_id = NULL WHERE event_id = $1`, postered); err != nil {
		t.Fatal(err)
	}
	if report := framePass(t, d.worker(t, d.frames.client(t))); report.Made != 1 || d.frameOf(t, postered, posteredVideo.ID).frame == nil {
		t.Fatalf("after the poster was cleared: %+v", report)
	}
}

// posterImage stores an uploaded Event photo, unattached.
func (d *frameDatabase) posterImage(t *testing.T) uuid.UUID {
	t.Helper()
	expires := d.now.Add(24 * time.Hour)
	poster, err := d.store.Create(context.Background(), media.Media{
		Name: "kapak.jpg", Type: "image/jpeg", Kind: media.KindImage, Key: "images/" + uuid.NewString(), Size: 10,
		UploadedBy: d.uploader, Purpose: media.PurposeEventCover, Status: media.StatusPending, ExpiresAt: &expires,
		Visibility: media.VisibilityPublic, ServingPolicyApplied: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return poster.ID
}

// A frame that fails (the storage, a timeout) is tried again later, each
// failure in a row doubling the wait, and given up after the last try: the
// video then has no poster. A video the service takes no frame from is
// given up at once.
func TestPostgresFrameWorkerBacksOffAndGivesUp(t *testing.T) {
	d := newFrameDatabase(t)
	eventID, video := d.eventVideo(t, string(media.FaststartDone))
	w := d.worker(t, d.frames.client(t))
	d.frames.setAnswer(func(w http.ResponseWriter, _ mediaframe.Request) bool {
		problem(w, http.StatusBadGateway, mediaframe.CodeUpstream)
		return true
	})

	if report := framePass(t, w); report.Retried != 1 || report.Made != 0 {
		t.Fatalf("pass %+v", report)
	}
	f := d.frameOf(t, eventID, video.ID)
	if f.frame != nil || f.attempts != 1 || f.retryAt == nil || !f.retryAt.Equal(d.now.Add(time.Minute).Truncate(time.Microsecond)) || f.claimed {
		t.Fatalf("after one failure: %+v", f)
	}
	if report := framePass(t, w); report.Claimed != 0 {
		t.Fatalf("tried again before its time: %+v", report)
	}
	d.now = d.now.Add(time.Minute)
	framePass(t, w)
	if f := d.frameOf(t, eventID, video.ID); f.attempts != 2 || !f.retryAt.Equal(d.now.Add(2*time.Minute).Truncate(time.Microsecond)) {
		t.Fatalf("after two failures: %+v", f)
	}

	// The last try: given up.
	if _, err := d.pool.Exec(context.Background(), `UPDATE event_videos SET frame_attempts = $3, frame_retry_at = NULL WHERE event_id = $1 AND media_id = $2`,
		eventID, video.ID, media.FrameMaxAttempts-1); err != nil {
		t.Fatal(err)
	}
	if report := framePass(t, w); report.Abandoned != 1 {
		t.Fatalf("the last try: %+v", report)
	}
	if f := d.frameOf(t, eventID, video.ID); f.state != "failed" || f.frame != nil || f.claimed {
		t.Fatalf("given up: %+v", f)
	}
	d.now = d.now.Add(24 * time.Hour)
	if report := framePass(t, w); report.Claimed != 0 {
		t.Fatalf("a video given up on was taken again: %+v", report)
	}

	// A video that gives no frame fails at once.
	other, otherVideo := d.eventVideo(t, string(media.FaststartDone))
	d.frames.setAnswer(func(w http.ResponseWriter, _ mediaframe.Request) bool {
		problem(w, http.StatusUnprocessableEntity, mediaframe.CodeNoFrame)
		return true
	})
	if report := framePass(t, w); report.Failed != 1 {
		t.Fatalf("no frame: %+v", report)
	}
	if f := d.frameOf(t, other, otherVideo.ID); f.state != "failed" || f.attempts != 1 || f.claimed {
		t.Fatalf("a video with no frame: %+v", f)
	}
}

// A frame service that cannot be reached, is busy, or refuses core's
// addresses (its allowlist does not name the storage) costs no video a
// try: the pass ends, every claim let go, and the worker waits before the
// next.
func TestPostgresFrameWorkerLetsGoWhileTheServiceIsDown(t *testing.T) {
	d := newFrameDatabase(t)
	eventID, video := d.eventVideo(t, string(media.FaststartDone))
	d.eventVideo(t, string(media.FaststartDone))
	for name, answer := range map[string]func(w http.ResponseWriter, _ mediaframe.Request) bool{
		"busy": func(w http.ResponseWriter, _ mediaframe.Request) bool {
			problem(w, http.StatusServiceUnavailable, mediaframe.CodeBusy)
			return true
		},
		"refusing core's address": func(w http.ResponseWriter, _ mediaframe.Request) bool {
			problem(w, http.StatusBadRequest, mediaframe.CodeURLNotAllowed)
			return true
		},
	} {
		d.frames.setAnswer(answer)
		report, err := d.worker(t, d.frames.client(t)).Pass(context.Background(), func(error) {})
		if !errors.Is(err, media.ErrFrameServiceDown) || report.Claimed != 1 || report.Released != 1 {
			t.Fatalf("%s: report %+v, err %v", name, report, err)
		}
		if f := d.frameOf(t, eventID, video.ID); f.attempts != 0 || f.retryAt != nil || f.claimed || f.state != "" {
			t.Fatalf("%s: %+v", name, f)
		}
	}
	d.frames.server.Close()
	report, err := d.worker(t, d.frames.client(t)).Pass(context.Background(), func(error) {})
	if !errors.Is(err, media.ErrFrameServiceDown) || report.Released != 1 {
		t.Fatalf("unreachable: report %+v, err %v", report, err)
	}
	if f := d.frameOf(t, eventID, video.ID); f.attempts != 0 || f.claimed {
		t.Fatalf("unreachable: %+v", f)
	}
}

// Two workers (a rolling deploy's two cores) take each video's frame once:
// a video one has claimed is left alone by the other.
func TestPostgresTwoFrameWorkersTakeEachFrameOnce(t *testing.T) {
	d := newFrameDatabase(t)
	eventID, _ := d.eventVideo(t, string(media.FaststartDone))
	for range 3 {
		d.addVideo(t, eventID, string(media.FaststartDone))
	}
	// The service holds each request until another arrives (or a moment
	// passes), so the two passes overlap.
	var waiting atomic.Int32
	arrived := make(chan struct{}, 8)
	frame := testFrameJPEG(t, 640, 360)
	d.frames.setAnswer(func(w http.ResponseWriter, _ mediaframe.Request) bool {
		if waiting.Add(1) == 1 {
			select {
			case <-arrived:
			case <-time.After(2 * time.Second):
			}
		} else {
			arrived <- struct{}{}
		}
		waiting.Add(-1)
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write(frame)
		return true
	})
	var wg sync.WaitGroup
	reports := make([]media.FrameReport, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			reports[i], _ = d.worker(t, d.frames.client(t)).Pass(context.Background(), func(err error) { t.Logf("frame: %v", err) })
		}()
	}
	wg.Wait()
	if made := reports[0].Made + reports[1].Made; made != 4 || len(d.frames.requests()) != 4 {
		t.Fatalf("two workers made %d frames and asked %d times (%+v)", made, len(d.frames.requests()), reports)
	}
	var frames, distinct int
	if err := d.pool.QueryRow(context.Background(), `SELECT count(frame_media_id), count(DISTINCT frame_media_id) FROM event_videos WHERE event_id = $1`, eventID).
		Scan(&frames, &distinct); err != nil || frames != 4 || distinct != 4 {
		t.Fatalf("%d frames, %d distinct (%v)", frames, distinct, err)
	}
	var stored int
	if err := d.pool.QueryRow(context.Background(), `SELECT count(*) FROM media WHERE purpose = 'video_frame'`).Scan(&stored); err != nil || stored != 4 {
		t.Fatalf("%d frame Media stored (%v)", stored, err)
	}
}

// Removing the video from the Event detaches its frame (purged 30 days
// later), as it does its poster. A video whose key changes (faststart
// moved it) keeps its frame.
func TestPostgresAVideosFrameGoesWithTheVideo(t *testing.T) {
	d := newFrameDatabase(t)
	ctx := context.Background()
	eventID, video := d.eventVideo(t, string(media.FaststartMoved))
	w := d.worker(t, d.frames.client(t))
	framePass(t, w)
	frameID := d.frameOf(t, eventID, video.ID).frame
	if frameID == nil {
		t.Fatal("no frame")
	}

	if _, err := d.pool.Exec(ctx, `UPDATE media SET file_url = $2, video_faststart = 'done' WHERE id = $1`, video.ID, strings.TrimSuffix(video.Key, ".mp4")+".fs.0123456789abcdef0123456789abcdef.mp4"); err != nil {
		t.Fatal(err)
	}
	if report := framePass(t, w); report.Claimed != 0 || *d.frameOf(t, eventID, video.ID).frame != *frameID || d.get(t, *frameID).Status != media.StatusAttached {
		t.Fatalf("after the video's key changed: %+v", report)
	}

	if _, err := d.pool.Exec(ctx, `DELETE FROM event_videos WHERE event_id = $1 AND media_id = $2`, eventID, video.ID); err != nil {
		t.Fatal(err)
	}
	frame := d.get(t, *frameID)
	if frame.Status != media.StatusDetached || frame.ExpiresAt == nil || d.attachedAs(t, frame.ID, eventID, "event_video_frame") != 0 {
		t.Fatalf("after the video was removed: %+v", frame)
	}
}

// A frame whose video left the Event while the frame was taken is not
// linked: its Media expires at once, and the expiry cleanup deletes its
// objects.
func TestPostgresAFrameWhoseVideoWentIsDiscarded(t *testing.T) {
	d := newFrameDatabase(t)
	ctx := context.Background()
	eventID, video := d.eventVideo(t, string(media.FaststartDone))
	frame := testFrameJPEG(t, 1280, 720)
	d.frames.setAnswer(func(w http.ResponseWriter, _ mediaframe.Request) bool {
		if _, err := d.pool.Exec(ctx, `DELETE FROM event_videos WHERE event_id = $1 AND media_id = $2`, eventID, video.ID); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write(frame)
		return true
	})
	if report := framePass(t, d.worker(t, d.frames.client(t))); report.Discarded != 1 || report.Made != 0 {
		t.Fatalf("pass %+v", report)
	}
	var id uuid.UUID
	var key string
	var expired bool
	if err := d.pool.QueryRow(ctx, `SELECT id, file_url, expires_at <= $1 FROM media WHERE purpose = 'video_frame'`, d.now).Scan(&id, &key, &expired); err != nil || !expired {
		t.Fatalf("the discarded frame: expired %v (%v)", expired, err)
	}
	if _, err := media.PurgeExpired(ctx, d.store, d.r2, d.now, func(err error) { t.Error(err) }); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{key, key + "/card.jpg", key + "/page.jpg"} {
		if _, ok := d.fake.Object("media", k); ok {
			t.Errorf("%s is still stored", k)
		}
	}
}

// panickingFrames is a frame service whose client panics (a bug).
type panickingFrames struct{ calls atomic.Int32 }

func (p *panickingFrames) Frame(context.Context, string, time.Duration) ([]byte, error) {
	if p.calls.Add(1) == 1 {
		panic("a bug")
	}
	return nil, errors.New("no frame here")
}

// A step that panics fails its video (no poster) and the pass goes on to
// the next.
func TestPostgresAFrameStepThatPanicsFailsItsVideo(t *testing.T) {
	d := newFrameDatabase(t)
	first, firstVideo := d.eventVideo(t, string(media.FaststartDone))
	second, secondVideo := d.eventVideo(t, string(media.FaststartDone))
	frames := &panickingFrames{}
	report := framePass(t, d.worker(t, frames))
	if report.Panicked != 1 || report.Claimed != 2 || frames.calls.Load() != 2 {
		t.Fatalf("pass %+v, %d calls", report, frames.calls.Load())
	}
	failed, retried := d.frameOf(t, first, firstVideo.ID), d.frameOf(t, second, secondVideo.ID)
	if first.String() > second.String() {
		failed, retried = retried, failed
	}
	if failed.state != "failed" || failed.claimed || retried.state != "" || retried.attempts != 1 {
		t.Fatalf("panicked on %+v, next %+v", failed, retried)
	}
}

// Presigned GETs work as the frame service uses them: a plain GET of the
// address, nothing else to go on.
func TestPostgresPresignedGetReadsTheObject(t *testing.T) {
	r2, fake := multipartR2(t)
	ctx := context.Background()
	if err := r2.Put(ctx, "videos/a.mp4", []byte("video"), media.BlobMetadata{ContentType: "video/mp4"}); err != nil {
		t.Fatal(err)
	}
	address, err := r2.PresignGet(ctx, "videos/a.mp4", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(address, fake.URL+"/media/videos/a.mp4?") || !strings.Contains(address, "X-Amz-Expires=60") {
		t.Fatalf("address %q", address)
	}
	resp, err := http.Get(address)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if body, _ := io.ReadAll(resp.Body); string(body) != "video" {
		t.Fatalf("read %q", body)
	}
}
