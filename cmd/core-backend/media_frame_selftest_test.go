package main

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/media/s3test"
	"github.com/skylab-kulubu/core-backend/internal/mediaframe"
)

// fakeFrameService reads the video by the address it is given and answers
// a 320x180 blue JPEG (the sample video's frame at 1 s), or the status
// given.
type fakeFrameService struct {
	*httptest.Server
	mu      sync.Mutex
	asked   []mediaframe.Request
	read    []byte
	status  int
	picture color.Color
}

func newFakeFrameService(t *testing.T) *fakeFrameService {
	t.Helper()
	f := &fakeFrameService{picture: color.RGBA{B: 220, A: 255}}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req mediaframe.Request
		json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.asked = append(f.asked, req)
		status, picture := f.status, f.picture
		f.mu.Unlock()
		if status != 0 {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(status)
			io.WriteString(w, `{"code":"no_frame"}`)
			return
		}
		resp, err := http.Get(req.URL)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		f.mu.Lock()
		f.read = body
		f.mu.Unlock()
		img := image.NewRGBA(image.Rect(0, 0, 320, 180))
		for x := range 320 {
			for y := range 180 {
				img.Set(x, y, picture)
			}
		}
		w.Header().Set("Content-Type", "image/jpeg")
		jpeg.Encode(w, img, nil)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeFrameService) addr() string { return strings.TrimPrefix(f.URL, "http://") }

// selfTestEnv is core's R2 environment on the fake bucket, and the frame
// service's address.
func selfTestEnv(bucket *s3test.Server, frameAddr string) map[string]string {
	return map[string]string{
		"R2_ENDPOINT": bucket.URL, "R2_BUCKET": "media", "R2_ACCESS_KEY": "ak", "R2_SECRET_KEY": "sk",
		"MEDIA_FRAME_ADDR": frameAddr,
	}
}

// The deploy check: inside core's container, a tiny sample video stored
// under a temporary key, read by the frame service through a presigned GET,
// gives its frame at one second. The key is deleted afterwards. It exits 0
// only then.
func TestMediaFrameSelfTestPassesOnlyWithTheFrameOfTheSample(t *testing.T) {
	bucket := s3test.New(t)
	frames := newFakeFrameService(t)
	var out, errOut bytes.Buffer
	if code := runMediaFrameSelfTest(nil, env(selfTestEnv(bucket, frames.addr())), &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s%s", code, out.String(), errOut.String())
	}
	if len(frames.asked) != 1 || frames.asked[0].AtMillis != 1000 {
		t.Fatalf("the frame service was asked %+v", frames.asked)
	}
	address, _ := url.Parse(frames.asked[0].URL)
	if !strings.HasPrefix(address.Path, "/media/pending/selftest/frame-") || address.Query().Get("X-Amz-Signature") == "" {
		t.Fatalf("the sample's address %q", frames.asked[0].URL)
	}
	if !bytes.HasPrefix(frames.read[4:], []byte("ftyp")) {
		t.Fatalf("the frame service read %q, not the sample MP4", frames.read[:min(16, len(frames.read))])
	}
	if keys := bucket.Keys("media"); len(keys) != 0 {
		t.Fatalf("the sample was left at %v", keys)
	}
	for _, want := range []string{frames.addr(), "320x180", "FRAME OK"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String()+errOut.String(), "Signature") {
		t.Fatal("the output carries the presigned address")
	}

	// -addr overrides the environment: the wizard tests a service before
	// core's env names it.
	out.Reset()
	noAddr := selfTestEnv(bucket, "")
	if code := runMediaFrameSelfTest([]string{"-addr", frames.addr()}, env(noAddr), &out, &errOut); code != 0 {
		t.Fatalf("-addr: exit %d: %s", code, errOut.String())
	}

	// A frame that is not the sample's second second (red: its first).
	frames.mu.Lock()
	frames.picture = color.RGBA{R: 220, A: 255}
	frames.mu.Unlock()
	errOut.Reset()
	if code := runMediaFrameSelfTest(nil, env(selfTestEnv(bucket, frames.addr())), &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "1 s") {
		t.Fatalf("the wrong frame: exit %d: %s", code, errOut.String())
	}

	frames.mu.Lock()
	frames.status = http.StatusUnprocessableEntity
	frames.mu.Unlock()
	errOut.Reset()
	if code := runMediaFrameSelfTest(nil, env(selfTestEnv(bucket, frames.addr())), &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "no_frame") {
		t.Fatalf("no frame: exit %d: %s", code, errOut.String())
	}
	if keys := bucket.Keys("media"); len(keys) != 0 {
		t.Fatalf("a failed test left the sample at %v", keys)
	}

	frames.Close()
	errOut.Reset()
	if code := runMediaFrameSelfTest(nil, env(selfTestEnv(bucket, frames.addr())), &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "cannot be reached") {
		t.Fatalf("a service that is down: exit %d: %s", code, errOut.String())
	}
}

func TestMediaFrameSelfTestNeedsAnAddressAndR2(t *testing.T) {
	bucket := s3test.New(t)
	var out, errOut bytes.Buffer
	if code := runMediaFrameSelfTest(nil, env(selfTestEnv(bucket, "")), &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "MEDIA_FRAME_ADDR") {
		t.Fatalf("no address: exit %d: %s", code, errOut.String())
	}
	errOut.Reset()
	if code := runMediaFrameSelfTest([]string{"-addr", "media-frame"}, env(selfTestEnv(bucket, "")), &out, &errOut); code != 2 {
		t.Fatalf("a malformed address: exit %d: %s", code, errOut.String())
	}
	errOut.Reset()
	if code := runMediaFrameSelfTest([]string{"-addr", "media-frame:8080"}, env(nil), &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "R2") {
		t.Fatalf("no R2: exit %d: %s", code, errOut.String())
	}
}
