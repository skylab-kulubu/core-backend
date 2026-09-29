package mediaframe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNewClientTakesHostAndPort(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"media-frame:8080", "sky-lab-sandbox-media-frame-abc123:8080", "127.0.0.1:9"} {
		if _, err := NewClient(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "media-frame", "http://media-frame:8080", "media-frame:0", "media-frame:70000", "user@media-frame:8080", "media-frame:8080/frame", ":8080"} {
		if _, err := NewClient(bad); err == nil {
			t.Errorf("%q: accepted", bad)
		}
	}
}

// clientFor is a client of a fake frame service answering with handle.
func clientFor(t *testing.T, handle http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handle)
	t.Cleanup(server.Close)
	client, err := NewClient(strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func problemHandler(status int, code string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(status)
		fmt.Fprintf(w, `{"status":%d,"code":%q,"detail":"why","upstreamStatus":404}`, status, code)
	}
}

// decodeStrict decodes one JSON value into v, refusing unknown fields.
func decodeStrict(r io.Reader, v any) error {
	decoder := json.NewDecoder(r)
	decoder.DisallowUnknownFields()
	return decoder.Decode(v)
}

// The client posts the video's address and the time, and answers the JPEG;
// the service's problems come back as a *Problem that tells a video without
// a frame (ErrNoFrame) and a busy or unreachable service (ErrUnavailable)
// from the rest.
func TestTheClientAsksForAFrame(t *testing.T) {
	t.Parallel()
	frame := testJPEG(t)
	var got Request
	client := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/frame" || r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "wrong", http.StatusBadRequest)
			return
		}
		if err := decodeStrict(r.Body, &got); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write(frame)
	})
	answer, err := client.Frame(context.Background(), "https://bucket.r2.test/v.mp4?sig=1", 1500*time.Millisecond)
	if err != nil || string(answer) != string(frame) {
		t.Fatalf("frame %d bytes, %v", len(answer), err)
	}
	if got.URL != "https://bucket.r2.test/v.mp4?sig=1" || got.AtMillis != 1500 {
		t.Fatalf("the service got %+v", got)
	}

	for _, c := range []struct {
		status  int
		code    string
		is      error
		isNot   error
		upprobe int
	}{
		{http.StatusUnprocessableEntity, CodeNoFrame, ErrNoFrame, ErrUnavailable, 404},
		{http.StatusServiceUnavailable, CodeBusy, ErrUnavailable, ErrNoFrame, 404},
		{http.StatusBadGateway, CodeUpstream, nil, ErrNoFrame, 404},
		{http.StatusGatewayTimeout, CodeTimeout, nil, ErrUnavailable, 404},
	} {
		_, err := clientFor(t, problemHandler(c.status, c.code)).Frame(context.Background(), "https://b.test/v", 0)
		var problem *Problem
		if !errors.As(err, &problem) || problem.Status != c.status || problem.Code != c.code || problem.UpstreamStatus != c.upprobe {
			t.Errorf("%d %s: %v", c.status, c.code, err)
			continue
		}
		if c.is != nil && !errors.Is(err, c.is) {
			t.Errorf("%d %s: not %v", c.status, c.code, c.is)
		}
		if errors.Is(err, c.isNot) {
			t.Errorf("%d %s: is %v", c.status, c.code, c.isNot)
		}
	}
}

// A service that cannot be reached is unavailable; an answer that is not a
// JPEG, or larger than a frame can be, is an error.
func TestTheClientRefusesWhatIsNotAFrame(t *testing.T) {
	t.Parallel()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	listener.Close()
	down, err := NewClient(addr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := down.Frame(context.Background(), "https://b.test/v", 0); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("a service that is down: %v", err)
	}

	for name, handle := range map[string]http.HandlerFunc{
		"not a JPEG": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "image/jpeg")
			w.Write([]byte("GIF89a"))
		},
		"another type": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			w.Write(testJPEG(t))
		},
		"too large": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "image/jpeg")
			w.Write(append([]byte{0xFF, 0xD8, 0xFF}, make([]byte, DefaultMaxOutputBytes)...))
		},
		"a problem without a body": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		},
	} {
		if frame, err := clientFor(t, handle).Frame(context.Background(), "https://b.test/v", 0); err == nil || errors.Is(err, ErrNoFrame) || errors.Is(err, ErrUnavailable) {
			t.Errorf("%s: %d bytes, %v", name, len(frame), err)
		}
	}
}
