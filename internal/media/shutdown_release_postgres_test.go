package media_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/skylab-kulubu/core-backend/internal/faststart/mp4test"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/mediaframe"
	"github.com/skylab-kulubu/core-backend/internal/transit"
)

// Core's shutdown cancels the workers' context (cmd/core-backend/serve.go).
// A step it cuts short is no failure of the Media: its claim is let go
// without an attempt counted or a failure reported, and the next pass (on
// this task or the next) takes it at once. Before core stopped gracefully,
// the process died and the claim's lease ran out, costing nothing either.

// cancellingOpens stops core (cancels the pass) as the scan reads the
// object, and fails as a read cut off does.
type cancellingOpens struct {
	*media.MemoryBlob
	cancel context.CancelFunc
}

func (c *cancellingOpens) Open(ctx context.Context, _ string) (io.ReadCloser, error) {
	c.cancel()
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestPostgresAScanCutShortByShutdownCostsNoAttempt(t *testing.T) {
	d := newScanDatabase(t)
	waiting := d.answer(t, pdfFile())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cut := mustScanWorker(t, media.ScanWorkerConfig{
		Store: d.store, Scanner: d.client, Public: d.blobs,
		Private: media.NewPrivateStorage(&cancellingOpens{MemoryBlob: d.private, cancel: cancel}, transit.New(d.bao.Config())),
		Now:     func() time.Time { return d.now },
	})
	var reported []error
	report, err := cut.Pass(ctx, func(err error) { reported = append(reported, err) })
	if !errors.Is(err, context.Canceled) || report.Failed != 0 || len(reported) != 0 {
		t.Fatalf("pass: report %+v, err %v, reported %v", report, err, reported)
	}
	var claimed bool
	if err := d.pool.QueryRow(context.Background(), `SELECT scan_claim_id IS NOT NULL FROM media WHERE id = $1`, waiting.ID).Scan(&claimed); err != nil {
		t.Fatal(err)
	}
	if got := d.get(t, waiting.ID); got.Status != media.StatusScanning || attempts(t, d, waiting.ID) != 0 || claimed {
		t.Fatalf("after shutdown: %s, %d attempts, claimed %v", got.Status, attempts(t, d, waiting.ID), claimed)
	}
	if report := d.pass(t); report.Clean != 1 {
		t.Fatalf("the next pass: %+v", report)
	}
}

// cancellingSize stops core as the rewrite sizes the video.
type cancellingSize struct {
	media.FaststartStorage
	cancel context.CancelFunc
}

func (c *cancellingSize) Size(ctx context.Context, _ string) (int64, error) {
	c.cancel()
	<-ctx.Done()
	return 0, ctx.Err()
}

func TestPostgresAFaststartCutShortByShutdownCostsNoAttempt(t *testing.T) {
	d := newFaststartDatabase(t)
	m := mp4test.Movie{Tracks: [][]int{{0}}, Chunks: [][]byte{mp4test.Chunk("c0", 1<<10)}}
	file, _ := m.Build()
	v := d.video(t, file)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var reported []error
	report, err := d.worker(t, &cancellingSize{FaststartStorage: d.r2, cancel: cancel}, d.clock).
		Pass(ctx, func(err error) { reported = append(reported, err) })
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if report.Retried+report.Abandoned+report.Refused != 0 || len(reported) != 0 {
		t.Fatalf("pass: report %+v, reported %v", report, reported)
	}
	attempts, retry := d.attempts(t, v.ID)
	var claimed bool
	if err := d.pool.QueryRow(context.Background(), `SELECT video_faststart_claim_id IS NOT NULL FROM media WHERE id = $1`, v.ID).Scan(&claimed); err != nil {
		t.Fatal(err)
	}
	if attempts != 0 || retry != nil || claimed || d.state(t, v.ID) != "" {
		t.Fatalf("after shutdown: attempts %d, retry %v, claimed %v, state %q", attempts, retry, claimed, d.state(t, v.ID))
	}
	if report := faststartPass(t, d.worker(t, d.r2, d.clock)); report.Claimed != 1 {
		t.Fatalf("the next pass: %+v", report)
	}
}

func TestPostgresAFrameCutShortByShutdownCostsNoAttempt(t *testing.T) {
	d := newFrameDatabase(t)
	eventID, video := d.eventVideo(t, string(media.FaststartDone))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.frames.setAnswer(func(w http.ResponseWriter, _ mediaframe.Request) bool {
		cancel()
		time.Sleep(200 * time.Millisecond)
		problem(w, http.StatusBadGateway, mediaframe.CodeUpstream)
		return true
	})
	var reported []error
	report, err := d.worker(t, d.frames.client(t)).Pass(ctx, func(err error) { reported = append(reported, err) })
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if report.Retried+report.Abandoned+report.Failed != 0 || len(reported) != 0 {
		t.Fatalf("pass: report %+v, reported %v", report, reported)
	}
	if f := d.frameOf(t, eventID, video.ID); f.attempts != 0 || f.retryAt != nil || f.claimed || f.state != "" {
		t.Fatalf("after shutdown: %+v", f)
	}
	d.frames.setAnswer(nil)
	if report := framePass(t, d.worker(t, d.frames.client(t))); report.Made != 1 {
		t.Fatalf("the next pass: %+v", report)
	}
}
