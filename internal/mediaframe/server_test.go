package mediaframe

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// videoHost is the storage host the tests allow; every request to it goes
// to the test's upstream, over TLS.
const videoHost = "example.com"

// upstream is a fake storage endpoint serving video over TLS, and a
// transport that reaches it for any host (the certificate names
// example.com).
type upstream struct {
	server *httptest.Server
	// requests are the requests it received: method, path and Range.
	mu       sync.Mutex
	requests []string
}

func newUpstream(t *testing.T, handler http.HandlerFunc) *upstream {
	t.Helper()
	u := &upstream{}
	u.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.requests = append(u.requests, r.Method+" "+r.URL.Path+" "+r.Header.Get("Range"))
		u.mu.Unlock()
		handler(w, r)
	}))
	t.Cleanup(u.server.Close)
	return u
}

func (u *upstream) seen() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return slices.Clone(u.requests)
}

// transport dials the upstream whatever host a request names, and trusts
// its certificate.
func (u *upstream) transport() http.RoundTripper {
	base := u.server.Client().Transport.(*http.Transport).Clone()
	addr := u.server.Listener.Addr().String()
	base.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	base.TLSClientConfig = base.TLSClientConfig.Clone()
	base.TLSClientConfig.MinVersion = tls.VersionTLS12
	return base
}

// serveVideo serves data as a video, answering Range requests.
func serveVideo(data []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "v.mp4", time.Time{}, bytes.NewReader(data))
	}
}

// testJPEG is a small JPEG.
func testJPEG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 16, 9))
	for i := range img.Pix {
		img.Pix[i] = 200
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, img, nil); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// argAfter is the argument after flag in args, and whether flag is there.
func argAfter(args []string, flag string) (string, bool) {
	i := slices.Index(args, flag)
	if i < 0 || i+1 >= len(args) {
		return "", false
	}
	return args[i+1], true
}

// readInput reads the whole input the ffmpeg arguments name, as ffmpeg
// would, and fails when the proxy does not serve it.
func readInput(ctx context.Context, args []string) ([]byte, error) {
	body, _, err := readInputTyped(ctx, args)
	return body, err
}

// readInputTyped is readInput, with the Content-Type the proxy answered.
func readInputTyped(ctx context.Context, args []string) ([]byte, string, error) {
	input, ok := argAfter(args, "-i")
	if !ok {
		return nil, "", errors.New("no -i")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, input, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Range", "bytes=0-")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return nil, "", fmt.Errorf("input answered %d", resp.StatusCode)
	}
	return body, resp.Header.Get("Content-Type"), nil
}

// frameService is the service under test over a fake ffmpeg.
func frameService(t *testing.T, up *upstream, run RunnerFunc, change func(*Config)) *httptest.Server {
	t.Helper()
	config := Config{
		AllowedHosts: []string{videoHost},
		FFmpeg:       run,
		Transport:    up.transport(),
		Timeout:      5 * time.Second,
		QueueWait:    5 * time.Second,
	}
	if change != nil {
		change(&config)
	}
	service, err := NewServer(config)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(service)
	t.Cleanup(server.Close)
	return server
}

type answer struct {
	status  int
	header  http.Header
	body    []byte
	problem map[string]any
}

func askFrame(t *testing.T, server *httptest.Server, body string) answer {
	t.Helper()
	resp, err := http.Post(server.URL+"/frame", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	a := answer{status: resp.StatusCode, header: resp.Header, body: raw}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "application/problem+json") {
		if err := json.Unmarshal(raw, &a.problem); err != nil {
			t.Fatalf("problem %s: %v", raw, err)
		}
	}
	return a
}

func frameBody(url string, at int64) string {
	return fmt.Sprintf(`{"url":%q,"atMillis":%d}`, url, at)
}

func (a answer) code() string {
	code, _ := a.problem["code"].(string)
	return code
}

// The service reads the video through its own loopback proxy, which reads
// the allowed https address with the same Range, and answers the one JPEG
// frame ffmpeg writes. ffmpeg is held to its limits: no stdin, loopback http
// only, the input read as an MP4 alone (the mov demuxer, its external
// references off) whatever type the storage names, a fast seek before the
// input, one frame, at most 1920 px, written to its standard output.
func TestTheServiceAnswersOneJPEGFrame(t *testing.T) {
	t.Parallel()
	video := []byte("not really a video, but the proxy does not care")
	up := newUpstream(t, serveVideo(video))
	frame := testJPEG(t)
	var gotArgs []string
	var read []byte
	var inputType string
	server := frameService(t, up, func(ctx context.Context, args []string, stdout, _ io.Writer) error {
		gotArgs = args
		var err error
		if read, inputType, err = readInputTyped(ctx, args); err != nil {
			return err
		}
		_, err = stdout.Write(frame)
		return err
	}, nil)

	got := askFrame(t, server, frameBody("https://"+videoHost+"/bucket/videos/a.mp4?X-Amz-Signature=s", 1500))
	if got.status != http.StatusOK || got.header.Get("Content-Type") != "image/jpeg" || !bytes.Equal(got.body, frame) {
		t.Fatalf("status %d type %q body %d bytes", got.status, got.header.Get("Content-Type"), len(got.body))
	}
	if !bytes.Equal(read, video) {
		t.Fatalf("ffmpeg read %q through the proxy", read)
	}
	if inputType != "application/octet-stream" {
		t.Fatalf("the proxy answered ffmpeg Content-Type %q, not the fixed application/octet-stream (the storage said video/mp4)", inputType)
	}
	format := slices.Index(gotArgs, "-f")
	if format < 0 || format+1 >= len(gotArgs) || gotArgs[format+1] != "mov" || format > slices.Index(gotArgs, "-i") {
		t.Fatalf("args %v: want -f mov before -i", gotArgs)
	}
	if drefs := slices.Index(gotArgs, "-enable_drefs"); drefs < 0 || gotArgs[drefs+1] != "0" || drefs > slices.Index(gotArgs, "-i") {
		t.Fatalf("args %v: want -enable_drefs 0 before -i", gotArgs)
	}
	if seen := up.seen(); len(seen) != 1 || seen[0] != "GET /bucket/videos/a.mp4 bytes=0-" {
		t.Fatalf("upstream saw %v", seen)
	}
	input, _ := argAfter(gotArgs, "-i")
	if !strings.HasPrefix(input, "http://127.0.0.1:") || strings.Contains(input, videoHost) || strings.Contains(input, "Signature") {
		t.Fatalf("ffmpeg's input %q is not the loopback proxy", input)
	}
	if at, _ := argAfter(gotArgs, "-ss"); at != "1.500" || slices.Index(gotArgs, "-ss") > slices.Index(gotArgs, "-i") {
		t.Fatalf("seek %q, args %v: want -ss 1.500 before -i", at, gotArgs)
	}
	for flag, want := range map[string]string{
		"-protocol_whitelist": "http,tcp",
		"-frames:v":           "1",
		"-c:v":                "mjpeg",
		"-threads":            "1",
	} {
		if got, _ := argAfter(gotArgs, flag); got != want {
			t.Errorf("%s %q, want %q (args %v)", flag, got, want, gotArgs)
		}
	}
	if output := gotArgs[slices.Index(gotArgs, "-i"):]; !slices.Contains(output, "image2pipe") {
		t.Errorf("args %v: want the frame written as image2pipe", gotArgs)
	}
	if !slices.Contains(gotArgs, "-nostdin") || gotArgs[len(gotArgs)-1] != "pipe:1" {
		t.Errorf("args %v: want -nostdin and the frame on pipe:1", gotArgs)
	}
	if scale, _ := argAfter(gotArgs, "-vf"); !strings.Contains(scale, "min(iw,1920)") || !strings.Contains(scale, "min(ih,1920)") {
		t.Errorf("scale %q does not cap at 1920 px", scale)
	}
}

// Nothing but an https address on an allowed host is read, and nothing runs
// for a refused request.
func TestTheServiceRefusesAddressesItMayNotRead(t *testing.T) {
	t.Parallel()
	up := newUpstream(t, serveVideo([]byte("v")))
	var runs atomic.Int32
	server := frameService(t, up, func(context.Context, []string, io.Writer, io.Writer) error {
		runs.Add(1)
		return nil
	}, nil)
	for _, c := range []struct {
		body, code string
	}{
		{frameBody("http://"+videoHost+"/v.mp4", 0), CodeURLNotAllowed},
		{frameBody("https://other.test/v.mp4", 0), CodeURLNotAllowed},
		{frameBody("https://127.0.0.1/v.mp4", 0), CodeURLNotAllowed},
		{frameBody("https://[::1]/v.mp4", 0), CodeURLNotAllowed},
		{frameBody("https://user:pw@"+videoHost+"/v.mp4", 0), CodeURLNotAllowed},
		{frameBody("https://"+videoHost+":8443/v.mp4", 0), CodeURLNotAllowed},
		{frameBody("https://"+videoHost+"/v.mp4", -1), CodeInvalidRequest},
		{frameBody("https://"+videoHost+"/v.mp4", maxAt.Milliseconds()+1), CodeInvalidRequest},
		{`{"url":"https://` + videoHost + `/v.mp4","atMillis":0,"extra":1}`, CodeInvalidRequest},
		{`{"url":"https://` + videoHost + `/v.mp4"} {}`, CodeInvalidRequest},
		{`not json`, CodeInvalidRequest},
		{`{"url":"https://` + videoHost + `/` + strings.Repeat("a", maxRequestBytes) + `"}`, CodeInvalidRequest},
	} {
		if got := askFrame(t, server, c.body); got.status != http.StatusBadRequest || got.code() != c.code {
			t.Errorf("%.80s: status %d code %q, want 400 %s", c.body, got.status, got.code(), c.code)
		}
	}
	resp, err := http.Get(server.URL + "/frame")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /frame: %d", resp.StatusCode)
	}
	if runs.Load() != 0 || len(up.seen()) != 0 {
		t.Fatalf("a refused request ran ffmpeg %d times and read %v", runs.Load(), up.seen())
	}
}

// A redirect is never followed, even to an allowed host: the frame fails as
// the storage's, and the address it names is never read.
func TestTheServiceFollowsNoRedirect(t *testing.T) {
	t.Parallel()
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere.mp4" {
			w.Write([]byte("elsewhere"))
			return
		}
		http.Redirect(w, r, "https://"+videoHost+"/elsewhere.mp4", http.StatusFound)
	})
	server := frameService(t, up, func(ctx context.Context, args []string, stdout, _ io.Writer) error {
		if _, err := readInput(ctx, args); err != nil {
			return err
		}
		_, err := stdout.Write(testJPEG(t))
		return err
	}, nil)
	got := askFrame(t, server, frameBody("https://"+videoHost+"/v.mp4", 0))
	if got.status != http.StatusBadGateway || got.code() != CodeUpstream {
		t.Fatalf("redirect: status %d code %q body %s", got.status, got.code(), got.body)
	}
	for _, seen := range up.seen() {
		if strings.Contains(seen, "elsewhere") {
			t.Fatalf("the redirect was followed: %v", up.seen())
		}
	}
}

// The storage answering an error fails the frame as the storage's (502,
// with its status): the video may be there later. A video ffmpeg cannot
// take a frame from is the video's (422): trying again does not help.
func TestTheServiceTellsStorageFailuresFromVideosWithoutAFrame(t *testing.T) {
	t.Parallel()
	missing := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "NoSuchKey", http.StatusNotFound) })
	server := frameService(t, missing, func(ctx context.Context, args []string, _, _ io.Writer) error {
		_, err := readInput(ctx, args)
		return err
	}, nil)
	got := askFrame(t, server, frameBody("https://"+videoHost+"/v.mp4", 0))
	if got.status != http.StatusBadGateway || got.code() != CodeUpstream || got.problem["upstreamStatus"] != float64(404) {
		t.Fatalf("missing object: status %d problem %v", got.status, got.problem)
	}

	up := newUpstream(t, serveVideo([]byte("garbage")))
	for name, run := range map[string]RunnerFunc{
		"ffmpeg fails": func(ctx context.Context, args []string, _, stderr io.Writer) error {
			readInput(ctx, args)
			io.WriteString(stderr, "Invalid data found when processing input")
			return errors.New("exit status 1")
		},
		"ffmpeg writes no frame": func(context.Context, []string, io.Writer, io.Writer) error { return nil },
		"ffmpeg writes no JPEG": func(_ context.Context, _ []string, stdout, _ io.Writer) error {
			_, err := stdout.Write([]byte("GIF89a"))
			return err
		},
		"the frame is too large": func(_ context.Context, _ []string, stdout, _ io.Writer) error {
			_, err := stdout.Write(append([]byte{0xFF, 0xD8, 0xFF}, make([]byte, 64)...))
			return err
		},
	} {
		server := frameService(t, up, run, func(c *Config) { c.MaxOutputBytes = 32 })
		if got := askFrame(t, server, frameBody("https://"+videoHost+"/v.mp4", 0)); got.status != http.StatusUnprocessableEntity || got.code() != CodeNoFrame {
			t.Errorf("%s: status %d code %q", name, got.status, got.code())
		}
	}
}

// A storage connection that breaks while the video is read fails the frame
// as the storage's (502), not as the video's.
func TestTheServiceTellsABrokenStorageReadFromAVideoWithoutAFrame(t *testing.T) {
	t.Parallel()
	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "100000")
		w.WriteHeader(http.StatusOK)
		w.Write(bytes.Repeat([]byte("v"), 1000))
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	})
	server := frameService(t, up, func(ctx context.Context, args []string, _, _ io.Writer) error {
		_, err := readInput(ctx, args)
		return err
	}, nil)
	if got := askFrame(t, server, frameBody("https://"+videoHost+"/v.mp4", 0)); got.status != http.StatusBadGateway || got.code() != CodeUpstream {
		t.Fatalf("a broken read: status %d code %q %s", got.status, got.code(), got.body)
	}
}

// ffmpeg reads at most MaxInputBytes of the video: a fast seek into a
// faststart MP4 reads its moov and a few chunks, never the whole file.
func TestTheServiceStopsReadingPastItsInputBudget(t *testing.T) {
	t.Parallel()
	up := newUpstream(t, serveVideo(bytes.Repeat([]byte("v"), 1<<20)))
	var read int
	server := frameService(t, up, func(ctx context.Context, args []string, stdout, _ io.Writer) error {
		data, err := readInput(ctx, args)
		read = len(data)
		if err != nil {
			return err
		}
		_, err = stdout.Write(testJPEG(t))
		return err
	}, func(c *Config) { c.MaxInputBytes = 64 << 10 })
	got := askFrame(t, server, frameBody("https://"+videoHost+"/v.mp4", 0))
	if got.status != http.StatusUnprocessableEntity || got.code() != CodeNoFrame {
		t.Fatalf("status %d code %q", got.status, got.code())
	}
	if read > 64<<10 {
		t.Fatalf("ffmpeg read %d bytes, over the 64 KiB budget", read)
	}
}

// A clip shorter than the time asked for has no frame there: the service
// takes its first frame instead.
func TestAShortClipGivesItsFirstFrame(t *testing.T) {
	t.Parallel()
	up := newUpstream(t, serveVideo([]byte("short")))
	frame := testJPEG(t)
	var seeks []string
	server := frameService(t, up, func(_ context.Context, args []string, stdout, _ io.Writer) error {
		at, _ := argAfter(args, "-ss")
		seeks = append(seeks, at)
		if at != "0.000" {
			return nil
		}
		_, err := stdout.Write(frame)
		return err
	}, nil)
	got := askFrame(t, server, frameBody("https://"+videoHost+"/v.mp4", 1000))
	if got.status != http.StatusOK || !bytes.Equal(got.body, frame) || !slices.Equal(seeks, []string{"1.000", "0.000"}) {
		t.Fatalf("status %d, seeks %v", got.status, seeks)
	}
}

// ffmpeg runs at most Timeout: it is stopped, and the frame fails as a
// timeout (504).
func TestTheServiceStopsFFmpegAtItsTimeout(t *testing.T) {
	t.Parallel()
	up := newUpstream(t, serveVideo([]byte("v")))
	stopped := make(chan struct{})
	server := frameService(t, up, func(ctx context.Context, _ []string, _, _ io.Writer) error {
		<-ctx.Done()
		close(stopped)
		return ctx.Err()
	}, func(c *Config) { c.Timeout = 100 * time.Millisecond })
	start := time.Now()
	got := askFrame(t, server, frameBody("https://"+videoHost+"/v.mp4", 0))
	if got.status != http.StatusGatewayTimeout || got.code() != CodeTimeout {
		t.Fatalf("status %d code %q", got.status, got.code())
	}
	select {
	case <-stopped:
	default:
		t.Fatal("ffmpeg was not stopped")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("answered after %s", elapsed)
	}
}

// One ffmpeg runs at a time. A request that finds it busy waits in a small
// queue; a request that finds the queue full, or waits longer than
// QueueWait, is answered 503 busy with Retry-After, and runs nothing.
func TestTheServiceRunsOneFFmpegAtATime(t *testing.T) {
	t.Parallel()
	up := newUpstream(t, serveVideo([]byte("v")))
	release := make(chan struct{})
	entered := make(chan struct{}, 4)
	var running, most atomic.Int32
	frame := testJPEG(t)
	server := frameService(t, up, func(_ context.Context, _ []string, stdout, _ io.Writer) error {
		now := running.Add(1)
		defer running.Add(-1)
		for {
			m := most.Load()
			if now <= m || most.CompareAndSwap(m, now) {
				break
			}
		}
		entered <- struct{}{}
		<-release
		_, err := stdout.Write(frame)
		return err
	}, func(c *Config) { c.Queue = 1; c.QueueWait = 10 * time.Second })

	results := make(chan answer, 2)
	go func() { results <- askFrame(t, server, frameBody("https://"+videoHost+"/a.mp4", 0)) }()
	<-entered
	// The second waits in the queue.
	go func() { results <- askFrame(t, server, frameBody("https://"+videoHost+"/b.mp4", 0)) }()
	waitFor(t, func() bool { return queued(server) == 1 })
	// The third finds the queue full.
	busy := askFrame(t, server, frameBody("https://"+videoHost+"/c.mp4", 0))
	if busy.status != http.StatusServiceUnavailable || busy.code() != CodeBusy || busy.header.Get("Retry-After") == "" {
		t.Fatalf("a full queue: status %d code %q Retry-After %q", busy.status, busy.code(), busy.header.Get("Retry-After"))
	}
	close(release)
	for range 2 {
		if got := <-results; got.status != http.StatusOK {
			t.Fatalf("a queued request: status %d %s", got.status, got.body)
		}
	}
	if most.Load() != 1 {
		t.Fatalf("%d ffmpeg ran at once", most.Load())
	}

	// A request that waits past QueueWait is busy too.
	hold, holding := make(chan struct{}), make(chan struct{}, 1)
	slow := frameService(t, up, func(_ context.Context, _ []string, stdout, _ io.Writer) error {
		holding <- struct{}{}
		<-hold
		_, err := stdout.Write(frame)
		return err
	}, func(c *Config) { c.Queue = 1; c.QueueWait = 100 * time.Millisecond })
	first := make(chan answer, 1)
	go func() { first <- askFrame(t, slow, frameBody("https://"+videoHost+"/a.mp4", 0)) }()
	<-holding
	if got := askFrame(t, slow, frameBody("https://"+videoHost+"/b.mp4", 0)); got.status != http.StatusServiceUnavailable || got.code() != CodeBusy {
		t.Fatalf("a request that waited too long: status %d code %q", got.status, got.code())
	}
	close(hold)
	<-first
}

// queued asks the service how many requests wait for ffmpeg (its health).
func queued(server *httptest.Server) int {
	resp, err := http.Get(server.URL + "/health")
	if err != nil {
		return -1
	}
	defer resp.Body.Close()
	var health struct {
		Waiting int `json:"waiting"`
	}
	if json.NewDecoder(resp.Body).Decode(&health) != nil {
		return -1
	}
	return health.Waiting
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// GET /health answers 200 while the service runs.
func TestHealth(t *testing.T) {
	t.Parallel()
	up := newUpstream(t, serveVideo(nil))
	server := frameService(t, up, func(context.Context, []string, io.Writer, io.Writer) error { return nil }, nil)
	resp, err := http.Get(server.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var health map[string]any
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&health) != nil || health["status"] != "ok" {
		t.Fatalf("health: %d %v", resp.StatusCode, health)
	}
}

// The proxy ffmpeg reads serves only the video, by GET or HEAD.
func TestTheProxyServesOnlyTheVideo(t *testing.T) {
	t.Parallel()
	up := newUpstream(t, serveVideo([]byte("video")))
	var statuses []int
	server := frameService(t, up, func(ctx context.Context, args []string, stdout, _ io.Writer) error {
		input, _ := argAfter(args, "-i")
		base := input[:strings.LastIndex(input, "/")]
		for _, probe := range []struct{ method, url string }{
			{http.MethodGet, base + "/other"},
			{http.MethodPost, input},
			{http.MethodHead, input},
		} {
			req, _ := http.NewRequestWithContext(ctx, probe.method, probe.url, nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return err
			}
			resp.Body.Close()
			statuses = append(statuses, resp.StatusCode)
		}
		_, err := stdout.Write(testJPEG(t))
		return err
	}, nil)
	if got := askFrame(t, server, frameBody("https://"+videoHost+"/v.mp4", 0)); got.status != http.StatusOK {
		t.Fatalf("status %d %s", got.status, got.body)
	}
	if want := []int{http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusOK}; !slices.Equal(statuses, want) {
		t.Fatalf("the proxy answered %v, want %v", statuses, want)
	}
	if seen := up.seen(); len(seen) != 1 || !strings.HasPrefix(seen[0], "HEAD /v.mp4") {
		t.Fatalf("upstream saw %v, want the HEAD alone", seen)
	}
}
