package qr

import (
	"bytes"
	"testing"
)

func TestPNGEncodesContent(t *testing.T) {
	t.Parallel()
	png, err := PNG("https://skyl.app/club", 128)
	if err != nil {
		t.Fatal(err)
	}
	if len(png) < 8 || !bytes.Equal(png[:8], []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}) {
		t.Fatalf("not a png, len=%d", len(png))
	}
}

func TestSizeFromQuery(t *testing.T) {
	t.Parallel()
	if SizeFromQuery("") != DefaultSize {
		t.Fatal("empty")
	}
	if SizeFromQuery("512") != 512 {
		t.Fatal("512")
	}
	if SizeFromQuery("nope") != DefaultSize {
		t.Fatal("invalid")
	}
}
