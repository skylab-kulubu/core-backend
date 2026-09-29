package faststart_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/faststart"
)

// An MP4 made by ffmpeg (which writes its moov at the end unless told
// otherwise), rewritten, is one ffprobe reads without a complaint, with its
// moov first, and it decodes to exactly the same frames and audio. Only
// where ffmpeg and ffprobe are installed; skipped elsewhere.
func TestRewriteOfARealMP4DecodesTheSame(t *testing.T) {
	t.Parallel()
	for _, tool := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}
	dir := t.TempDir()
	in, out := filepath.Join(dir, "in.mp4"), filepath.Join(dir, "out.mp4")
	run(t, "ffmpeg", "-v", "error", "-f", "lavfi", "-i", "testsrc=duration=2:size=160x90:rate=15",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=2", "-c:v", "mpeg4", "-c:a", "aac", "-shortest", in)
	file, err := os.ReadFile(in)
	if err != nil {
		t.Fatal(err)
	}
	if top := types(boxes(t, file, 0, int64(len(file)))); slices.Index(top, "moov") < slices.Index(top, "mdat") {
		t.Fatalf("ffmpeg wrote %v: the fixture already has its moov first", top)
	}
	rewritten, _ := rewrite(t, file)
	if err := os.WriteFile(out, rewritten, 0o600); err != nil {
		t.Fatal(err)
	}
	if top := types(boxes(t, rewritten, 0, int64(len(rewritten)))); slices.Index(top, "moov") > slices.Index(top, "mdat") {
		t.Fatalf("rewritten %v: the moov is not first", top)
	}
	if complaints := run(t, "ffprobe", "-v", "error", "-show_entries", "stream=codec_type,nb_frames", "-of", "csv=p=0", out); !bytes.Equal(complaints, run(t, "ffprobe", "-v", "error", "-show_entries", "stream=codec_type,nb_frames", "-of", "csv=p=0", in)) {
		t.Fatalf("ffprobe reads the streams differently: %s", complaints)
	}
	decode := func(path string) []byte {
		return run(t, "ffmpeg", "-v", "error", "-i", path, "-map", "0", "-f", "framemd5", "-")
	}
	if a, b := decode(in), decode(out); !bytes.Equal(a, b) || len(a) == 0 {
		t.Fatal("the rewrite does not decode to the same frames and audio")
	}
	if _, err := faststart.Plan(context.Background(), memory(rewritten), int64(len(rewritten)), faststart.DefaultLimits); err != faststart.ErrAlreadyFaststart {
		t.Fatalf("planning the rewrite again: %v, want ErrAlreadyFaststart", err)
	}
}

// run runs a tool and returns its standard output; anything on its
// standard error fails the test.
func run(t *testing.T, name string, args ...string) []byte {
	t.Helper()
	var stdout, stderr bytes.Buffer
	cmd := exec.Command(name, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil || stderr.Len() > 0 {
		t.Fatalf("%s: %v: %s", name, err, stderr.Bytes())
	}
	return stdout.Bytes()
}
