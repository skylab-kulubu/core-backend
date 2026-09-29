package mediaframe

import (
	"bytes"
	"image"
	"image/jpeg"
	"net/http"
	"os/exec"
	"testing"
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
