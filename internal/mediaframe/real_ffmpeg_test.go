package mediaframe

import (
	"bytes"
	"image"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// realFFmpeg is the ffmpeg installed here; the test is skipped without one.
func realFFmpeg(t *testing.T) RunnerFunc {
	t.Helper()
	path, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is not installed")
	}
	return FFmpeg(path).(RunnerFunc)
}

// decodeFrame decodes the answer's JPEG.
func decodeFrame(t *testing.T, got answer) image.Image {
	t.Helper()
	if got.status != http.StatusOK || got.header.Get("Content-Type") != "image/jpeg" {
		t.Fatalf("status %d type %q body %.300s", got.status, got.header.Get("Content-Type"), got.body)
	}
	img, err := jpeg.Decode(bytes.NewReader(got.body))
	if err != nil {
		t.Fatalf("the frame is not a JPEG: %v", err)
	}
	return img
}

// isMostly reports whether the pixel at the image's centre is mostly the
// colour of channel 0 (red), 1 (green) or 2 (blue).
func isMostly(img image.Image, channel int) bool {
	b := img.Bounds()
	r, g, bl, _ := img.At(b.Min.X+b.Dx()/2, b.Min.Y+b.Dy()/2).RGBA()
	c := []uint32{r >> 8, g >> 8, bl >> 8}
	for i, v := range c {
		if i != channel && v+80 > c[channel] {
			return false
		}
	}
	return c[channel] > 150
}

// Against the ffmpeg installed here, through the service's own proxy over
// TLS: the sample video (red for its first second, blue for its second)
// gives its blue frame at 1 s, and its red first frame when asked past its
// end. A frame larger than 1920 px is scaled down to it. Something that is
// not a video gives no frame (422).
func TestRealFFmpegTakesTheFrameAskedFor(t *testing.T) {
	t.Parallel()
	run := realFFmpeg(t)
	video, err := SampleVideo(320, 180)
	if err != nil {
		t.Fatal(err)
	}
	up := newUpstream(t, serveVideo(video))
	server := frameService(t, up, run, nil)

	blue := decodeFrame(t, askFrame(t, server, frameBody("https://"+videoHost+"/v.mp4", 1000)))
	if b := blue.Bounds(); b.Dx() != 320 || b.Dy() != 180 || !isMostly(blue, 2) {
		t.Fatalf("the frame at 1 s is %v and not blue", b)
	}
	if red := decodeFrame(t, askFrame(t, server, frameBody("https://"+videoHost+"/v.mp4", 60000))); !isMostly(red, 0) {
		t.Fatal("the frame past the clip's end is not its first (red) frame")
	}

	large, err := SampleVideo(2400, 1350)
	if err != nil {
		t.Fatal(err)
	}
	bigServer := frameService(t, newUpstream(t, serveVideo(large)), run, nil)
	if b := decodeFrame(t, askFrame(t, bigServer, frameBody("https://"+videoHost+"/v.mp4", 0))).Bounds(); b.Dx() != 1920 || b.Dy() != 1080 {
		t.Fatalf("a 2400x1350 video's frame is %dx%d, want 1920x1080", b.Dx(), b.Dy())
	}

	garbage := frameService(t, newUpstream(t, serveVideo(bytes.Repeat([]byte("not a video "), 1000))), run, nil)
	if got := askFrame(t, garbage, frameBody("https://"+videoHost+"/v.mp4", 1000)); got.status != http.StatusUnprocessableEntity || got.code() != CodeNoFrame {
		t.Fatalf("not a video: status %d code %q %s", got.status, got.code(), got.body)
	}
}

// canary is an http server on the internal network the proxy must never
// let ffmpeg reach; it counts what it is asked.
type canary struct {
	server *httptest.Server
	hits   atomic.Int32
}

func newCanary(t *testing.T) *canary {
	t.Helper()
	c := &canary{}
	c.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		c.hits.Add(1)
		w.Header().Set("Content-Type", "video/mp2t")
		w.Write(bytes.Repeat([]byte{0x47}, 188))
	}))
	t.Cleanup(c.server.Close)
	return c
}

// playlist is an HLS playlist whose one segment is at segment.
func playlist(segment string) []byte {
	return []byte("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:1\n#EXTINF:1.0,\n" + segment + "\n#EXT-X-ENDLIST\n")
}

// ftypPrefixed is data behind a 16-byte ftyp box: what the upload's check of
// an MP4's first bytes lets through.
func ftypPrefixed(data []byte) []byte {
	return append([]byte{0, 0, 0, 16, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm', 0, 0, 2, 0}, data...)
}

// ffmpeg reads the video only as an MP4 (the mov demuxer, its external
// references off), whatever the content or the storage's type say: a
// playlist that names another http address on the internal network (which
// -protocol_whitelist http,tcp alone would let ffmpeg open) gives no frame
// (422), ffmpeg asks the proxy for the one video path only, and the other
// address is never asked.
func TestRealFFmpegReadsTheVideoOnlyAsAnMP4(t *testing.T) {
	t.Parallel()
	run := realFFmpeg(t)
	for name, c := range map[string]struct {
		body        []byte
		contentType string
	}{
		"an HLS playlist":                           {contentType: "application/vnd.apple.mpegurl"},
		"an HLS playlist behind an ftyp box":        {contentType: "application/vnd.apple.mpegurl"},
		"an HLS playlist served as video":           {contentType: "video/mp4"},
		"an ffconcat list behind an ftyp box":       {contentType: "application/octet-stream"},
		"an HLS playlist behind an ftyp box as mp4": {contentType: "video/mp4"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			other := newCanary(t)
			segment := other.server.URL + "/segment.ts"
			body := playlist(segment)
			switch {
			case strings.HasPrefix(name, "an ffconcat"):
				body = ftypPrefixed([]byte("ffconcat version 1.0\nfile '" + segment + "'\n"))
			case strings.Contains(name, "behind an ftyp box"):
				body = ftypPrefixed(body)
			}
			up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", c.contentType)
				http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(body))
			})
			server := frameService(t, up, run, nil)
			got := askFrame(t, server, frameBody("https://"+videoHost+"/videos/v.mp4", 1000))
			if got.status != http.StatusUnprocessableEntity || got.code() != CodeNoFrame {
				t.Errorf("status %d code %q %s", got.status, got.code(), got.body)
			}
			if hits := other.hits.Load(); hits != 0 {
				t.Errorf("ffmpeg opened the address the input named (%d requests)", hits)
			}
			for _, seen := range up.seen() {
				if _, path, _ := strings.Cut(seen, " "); !strings.HasPrefix(path, "/videos/v.mp4 ") {
					t.Errorf("the storage was asked %q", seen)
				}
			}
		})
	}
}
