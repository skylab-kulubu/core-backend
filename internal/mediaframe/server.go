// Package mediaframe is the video frame service (media redesign ticket 25,
// decision P1) and core's client for it.
//
// The service (cmd/media-frame) runs in its own container, next to ffmpeg,
// on the internal network only. POST /frame with {"url": <a presigned GET of
// a video>, "atMillis": <where>} answers one JPEG frame of the video. It
// reads the video only through a loopback proxy of its own, so ffmpeg never
// reaches the network itself: the proxy reads the one https address the
// request named, on an allowed host (the storage endpoint), follows no
// redirect, forwards the Range requests ffmpeg makes (a fast seek into a
// faststart MP4 reads its moov and a few chunks), and stops at a byte
// budget. ffmpeg runs one at a time, bounded in time, and writes its frame
// to a pipe, never to disk. See "Video frames" in docs/media-lifecycle.md.
package mediaframe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"
)

// Request is the body of POST /frame.
type Request struct {
	// URL is a short-lived presigned GET of the video, on an allowed host.
	URL string `json:"url"`
	// AtMillis is where in the video the frame is taken. A clip shorter than
	// that gives its first frame.
	AtMillis int64 `json:"atMillis"`
}

// The problem codes the service answers (application/problem+json, member
// code).
const (
	// CodeInvalidRequest (400): the body is not a Request.
	CodeInvalidRequest = "invalid_request"
	// CodeURLNotAllowed (400): the address is not https on an allowed host.
	CodeURLNotAllowed = "url_not_allowed"
	// CodeBusy (503, Retry-After): ffmpeg is busy and the queue full, or the
	// request waited longer than QueueWait. Nothing ran.
	CodeBusy = "busy"
	// CodeTimeout (504): ffmpeg ran past Timeout and was stopped.
	CodeTimeout = "timeout"
	// CodeUpstream (502): the storage answered an error (upstreamStatus),
	// a redirect, or could not be reached. The video may be there later.
	CodeUpstream = "upstream_failed"
	// CodeNoFrame (422): ffmpeg took no frame from the video: not a video it
	// decodes, one that needs more read than MaxInputBytes, or a frame
	// larger than MaxOutputBytes. Trying again does not help.
	CodeNoFrame = "no_frame"
)

// Defaults of Config.
const (
	DefaultTimeout        = 30 * time.Second
	DefaultQueueWait      = 30 * time.Second
	DefaultQueue          = 2
	DefaultMaxInputBytes  = 256 << 20
	DefaultMaxOutputBytes = 8 << 20
	DefaultMaxDimension   = 1920
)

const (
	// maxRequestBytes bounds the body of POST /frame.
	maxRequestBytes = 8 << 10
	// maxAt is the latest a frame may be asked for.
	maxAt = 24 * time.Hour
	// busyRetryAfter is what a busy answer tells the caller to wait, in
	// seconds.
	busyRetryAfter = 5
)

// Runner runs ffmpeg with args, its standard output and error written to
// stdout and stderr; it stops ffmpeg when ctx ends. FFmpeg is the real one;
// tests pass a fake.
type Runner interface {
	Run(ctx context.Context, args []string, stdout, stderr io.Writer) error
}

// RunnerFunc is a Runner.
type RunnerFunc func(ctx context.Context, args []string, stdout, stderr io.Writer) error

// Run calls f.
func (f RunnerFunc) Run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	return f(ctx, args, stdout, stderr)
}

// Config is what the service needs. Only AllowedHosts and FFmpeg have no
// default.
type Config struct {
	// AllowedHosts are the only hosts a video is read from
	// (ParseAllowedHosts): the storage endpoint's.
	AllowedHosts []string
	FFmpeg       Runner
	// Transport reads the video from the storage; nil is an https
	// transport with bounded dial, handshake and header waits, and no
	// proxy from the environment.
	Transport http.RoundTripper
	// Timeout bounds a request's ffmpeg work, both runs when a short clip
	// is read again from its start.
	Timeout time.Duration
	// QueueWait bounds how long a request waits for ffmpeg.
	QueueWait time.Duration
	// Queue is how many requests may wait for ffmpeg while it runs.
	Queue int
	// MaxInputBytes bounds what one request reads of the video.
	MaxInputBytes int64
	// MaxOutputBytes bounds the frame.
	MaxOutputBytes int64
	// MaxDimension is the frame's longest side at most, in pixels.
	MaxDimension int
	// Logf logs one line per frame request (never its address); nil logs
	// nothing.
	Logf func(format string, args ...any)
}

// Server is the frame service's HTTP handler: POST /frame and GET /health.
type Server struct {
	config  Config
	mux     *http.ServeMux
	slot    chan struct{}
	waiting atomic.Int32
}

// NewServer makes the service.
func NewServer(config Config) (*Server, error) {
	if len(config.AllowedHosts) == 0 || config.FFmpeg == nil {
		return nil, errors.New("mediaframe: allowed hosts and ffmpeg are needed")
	}
	if config.Transport == nil {
		config.Transport = defaultTransport()
	}
	if config.Timeout <= 0 {
		config.Timeout = DefaultTimeout
	}
	if config.QueueWait <= 0 {
		config.QueueWait = DefaultQueueWait
	}
	if config.Queue <= 0 {
		config.Queue = DefaultQueue
	}
	if config.MaxInputBytes <= 0 {
		config.MaxInputBytes = DefaultMaxInputBytes
	}
	if config.MaxOutputBytes <= 0 {
		config.MaxOutputBytes = DefaultMaxOutputBytes
	}
	if config.MaxDimension <= 0 {
		config.MaxDimension = DefaultMaxDimension
	}
	if config.Logf == nil {
		config.Logf = func(string, ...any) {}
	}
	s := &Server{config: config, mux: http.NewServeMux(), slot: make(chan struct{}, 1)}
	s.mux.HandleFunc("POST /frame", s.frame)
	s.mux.HandleFunc("GET /health", s.health)
	return s, nil
}

// ServeHTTP serves POST /frame and GET /health.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"status": "ok", "running": len(s.slot), "waiting": s.waiting.Load()})
}

// outcome is how a frame request ended: the frame, or a problem.
type outcome struct {
	frame          []byte
	status         int
	code, detail   string
	upstreamStatus int
	// empty is a run that wrote no frame: at the time asked for, the clip
	// may have ended already.
	empty bool
}

func (s *Server) frame(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	result, read := s.answer(r)
	if result.frame != nil {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Content-Length", strconv.Itoa(len(result.frame)))
		w.WriteHeader(http.StatusOK)
		w.Write(result.frame)
		s.config.Logf("media-frame: 200, %d bytes, in %s, %d bytes read", len(result.frame), time.Since(start).Round(time.Millisecond), read)
		return
	}
	problem := map[string]any{"type": "about:blank", "title": http.StatusText(result.status), "status": result.status, "code": result.code}
	if result.detail != "" {
		problem["detail"] = result.detail
	}
	if result.upstreamStatus != 0 {
		problem["upstreamStatus"] = result.upstreamStatus
	}
	if result.code == CodeBusy {
		w.Header().Set("Retry-After", strconv.Itoa(busyRetryAfter))
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(result.status)
	json.NewEncoder(w).Encode(problem)
	s.config.Logf("media-frame: %d %s (%s), in %s, %d bytes read", result.status, result.code, result.detail, time.Since(start).Round(time.Millisecond), read)
}

func invalid(detail string) outcome {
	return outcome{status: http.StatusBadRequest, code: CodeInvalidRequest, detail: detail}
}

// answer reads the request, waits for ffmpeg and takes the frame; read is
// what it read of the video.
func (s *Server) answer(r *http.Request) (result outcome, read int64) {
	decoder := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxRequestBytes))
	decoder.DisallowUnknownFields()
	var req Request
	if err := decoder.Decode(&req); err != nil {
		return invalid("the body is not {\"url\", \"atMillis\"}"), 0
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return invalid("content after the request"), 0
	}
	at := time.Duration(req.AtMillis) * time.Millisecond
	if req.AtMillis < 0 || at > maxAt {
		return invalid(fmt.Sprintf("atMillis is outside 0..%d", maxAt.Milliseconds())), 0
	}
	target, err := checkURL(req.URL, s.config.AllowedHosts)
	if err != nil {
		return outcome{status: http.StatusBadRequest, code: CodeURLNotAllowed, detail: "only an https address on an allowed host is read"}, 0
	}
	release, ok := s.acquire(r.Context())
	if !ok {
		return outcome{status: http.StatusServiceUnavailable, code: CodeBusy, detail: "ffmpeg is busy; try again later"}, 0
	}
	defer release()

	ctx, cancel := context.WithTimeout(r.Context(), s.config.Timeout)
	defer cancel()
	proxy, err := startProxy(target, s.config.Transport, s.config.MaxInputBytes)
	if err != nil {
		return outcome{status: http.StatusInternalServerError, code: CodeUpstream, detail: "the loopback proxy did not start"}, 0
	}
	defer proxy.close()
	result = s.take(ctx, proxy, at)
	if result.empty && at > 0 {
		// A clip shorter than at has no frame there: its first.
		result = s.take(ctx, proxy, 0)
	}
	return result, proxy.read()
}

// acquire waits for ffmpeg, in the queue: false when the queue is full, the
// wait ran past QueueWait or the caller went.
func (s *Server) acquire(ctx context.Context) (release func(), ok bool) {
	release = func() { <-s.slot }
	select {
	case s.slot <- struct{}{}:
		return release, true
	default:
	}
	if s.waiting.Add(1) > int32(s.config.Queue) {
		s.waiting.Add(-1)
		return nil, false
	}
	defer s.waiting.Add(-1)
	timer := time.NewTimer(s.config.QueueWait)
	defer timer.Stop()
	select {
	case s.slot <- struct{}{}:
		return release, true
	case <-timer.C:
		return nil, false
	case <-ctx.Done():
		return nil, false
	}
}

var jpegStart = []byte{0xFF, 0xD8, 0xFF}

// take runs ffmpeg once for the frame at at, reading through proxy.
func (s *Server) take(ctx context.Context, proxy *upstreamProxy, at time.Duration) outcome {
	stdout := &cappedBuffer{max: s.config.MaxOutputBytes}
	stderr := &headBuffer{max: 2 << 10}
	runErr := s.config.FFmpeg.Run(ctx, ffmpegArgs(proxy.input(), at, s.config.MaxDimension), stdout, stderr)
	switch failure := proxy.failure(); {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return outcome{status: http.StatusGatewayTimeout, code: CodeTimeout, detail: fmt.Sprintf("ffmpeg ran past %s and was stopped", s.config.Timeout)}
	case errors.Is(failure, errInputBudget):
		return outcome{status: http.StatusUnprocessableEntity, code: CodeNoFrame, detail: failure.Error()}
	case failure != nil:
		result := outcome{status: http.StatusBadGateway, code: CodeUpstream, detail: failure.Error()}
		var status upstreamStatusError
		if errors.As(failure, &status) {
			result.upstreamStatus = int(status)
		}
		return result
	case stdout.overflow:
		return outcome{status: http.StatusUnprocessableEntity, code: CodeNoFrame, detail: fmt.Sprintf("the frame is larger than %d bytes", s.config.MaxOutputBytes)}
	case stdout.Len() == 0:
		return outcome{status: http.StatusUnprocessableEntity, code: CodeNoFrame, detail: "ffmpeg took no frame from the video: " + firstLine(stderr.Bytes()), empty: true}
	case runErr != nil || !bytes.HasPrefix(stdout.Bytes(), jpegStart):
		return outcome{status: http.StatusUnprocessableEntity, code: CodeNoFrame, detail: "ffmpeg wrote no JPEG frame: " + firstLine(stderr.Bytes())}
	}
	return outcome{frame: bytes.Clone(stdout.Bytes())}
}

// firstLine is the first line of ffmpeg's standard error, bounded: what the
// log says of a video ffmpeg refused. It names the loopback input, never the
// video's address.
func firstLine(stderr []byte) string {
	line, _, _ := bytes.Cut(bytes.TrimSpace(stderr), []byte("\n"))
	if len(line) > 200 {
		line = line[:200]
	}
	return string(line)
}

// cappedBuffer keeps at most max bytes; a write past it fails, which stops
// ffmpeg.
type cappedBuffer struct {
	bytes.Buffer
	max      int64
	overflow bool
}

var errOutputTooLarge = errors.New("mediaframe: output past its cap")

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if int64(b.Len()+len(p)) > b.max {
		b.overflow = true
		return 0, errOutputTooLarge
	}
	return b.Buffer.Write(p)
}

// headBuffer keeps the first max bytes written to it and drops the rest,
// never failing a write: ffmpeg's standard error must not stop it.
type headBuffer struct {
	bytes.Buffer
	max int
}

func (b *headBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.Len(); room > 0 {
		b.Buffer.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}
