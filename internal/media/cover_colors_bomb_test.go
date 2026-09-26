package media_test

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"runtime"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/media"
)

// canvasLieWebP is an extended WebP whose VP8X canvas says 1x1 while its VP8
// frame header says 8000x8000. image.DecodeConfig reports the canvas, so a
// pixel cap based on it lets the decoder allocate the frame.
func canvasLieWebP(t *testing.T) []byte {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString("UklGRkoAAABXRUJQVlA4WAoAAAAQAAAAAAAAAAAAQUxQSAwAAAARBxAR/Q9ERP8DAABWUDggGAAAABQBAJ0BKgEAAQAAAP4AAA3AAP7mtQAAAA==")
	if err != nil {
		t.Fatal(err)
	}
	i := bytes.Index(data, []byte("VP8 "))
	if i < 0 {
		t.Fatal("no VP8 chunk")
	}
	frame := data[i+8:]
	if !bytes.Equal(frame[3:6], []byte{0x9d, 0x01, 0x2a}) {
		t.Fatalf("no VP8 start code: % x", frame[:10])
	}
	binary.LittleEndian.PutUint16(frame[6:], 8000)
	binary.LittleEndian.PutUint16(frame[8:], 8000)
	return data
}

func TestExtractCoverColorsDoesNotDecodeAFrameLargerThanItsCanvas(t *testing.T) {
	data := canvasLieWebP(t)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	colors := media.ExtractCoverColors(data)
	runtime.ReadMemStats(&after)
	if len(colors) != 0 {
		t.Fatalf("colors = %v, want none", colors)
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 32<<20 {
		t.Fatalf("ExtractCoverColors allocated %d MiB for a %d-byte file", grew>>20, len(data))
	}
}
