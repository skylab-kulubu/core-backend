package mediaframe

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"testing"
	"time"
)

// take's switch sets the status a frame request answers, first match
// winning: the timeout, then the input budget, then any other failure of
// the storage (even under a frame ffmpeg wrote), then a frame past its cap,
// then no frame (the one outcome read again from the clip's start), then
// ffmpeg failing or writing no JPEG. Each row holds everything the rows
// below it test for, so an order that changes fails here.
func TestTakeAnswersTheFirstFailureInItsOrder(t *testing.T) {
	t.Parallel()
	jpegFrame := append([]byte{0xFF, 0xD8, 0xFF}, make([]byte, 8)...)
	for _, c := range []struct {
		name       string
		timedOut   bool
		failure    error
		output     []byte
		runErr     error
		status     int
		code       string
		upstream   int
		empty      bool
		frameShown bool
	}{
		{name: "timeout", timedOut: true, failure: errInputBudget, output: make([]byte, 64), runErr: errors.New("killed"),
			status: http.StatusGatewayTimeout, code: CodeTimeout},
		{name: "input budget", failure: errInputBudget, output: make([]byte, 64), runErr: errors.New("exit 1"),
			status: http.StatusUnprocessableEntity, code: CodeNoFrame},
		{name: "storage status under a frame", failure: upstreamStatusError(http.StatusNotFound), output: jpegFrame,
			status: http.StatusBadGateway, code: CodeUpstream, upstream: http.StatusNotFound},
		{name: "redirect", failure: errRedirect, output: make([]byte, 64), runErr: errors.New("exit 1"),
			status: http.StatusBadGateway, code: CodeUpstream},
		{name: "frame past its cap", output: make([]byte, 64), runErr: errors.New("exit 1"),
			status: http.StatusUnprocessableEntity, code: CodeNoFrame},
		{name: "no frame", runErr: errors.New("exit 1"),
			status: http.StatusUnprocessableEntity, code: CodeNoFrame, empty: true},
		{name: "ffmpeg failed after a JPEG", output: jpegFrame, runErr: errors.New("exit 1"),
			status: http.StatusUnprocessableEntity, code: CodeNoFrame},
		{name: "no JPEG", output: []byte("GIF89a"),
			status: http.StatusUnprocessableEntity, code: CodeNoFrame},
		{name: "a frame", output: jpegFrame, frameShown: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s, err := NewServer(Config{
				AllowedHosts:   []string{videoHost},
				MaxOutputBytes: 32,
				FFmpeg: RunnerFunc(func(_ context.Context, _ []string, stdout, _ io.Writer) error {
					if len(c.output) > 0 {
						stdout.Write(c.output)
					}
					return c.runErr
				}),
			})
			if err != nil {
				t.Fatal(err)
			}
			proxy, err := startProxy(&url.URL{Scheme: "https", Host: videoHost, Path: "/v.mp4"}, http.DefaultTransport, 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			defer proxy.close()
			if c.failure != nil {
				proxy.fail(c.failure)
			}
			ctx := context.Background()
			if c.timedOut {
				var cancel context.CancelFunc
				ctx, cancel = context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer cancel()
			}
			got := s.take(ctx, proxy, time.Second)
			if c.frameShown {
				if got.frame == nil || got.status != 0 {
					t.Fatalf("got %+v, want the frame", got)
				}
				return
			}
			if got.frame != nil || got.status != c.status || got.code != c.code || got.upstreamStatus != c.upstream || got.empty != c.empty {
				t.Fatalf("got status %d code %q upstream %d empty %v, want %d %q %d %v",
					got.status, got.code, got.upstreamStatus, got.empty, c.status, c.code, c.upstream, c.empty)
			}
		})
	}
}
