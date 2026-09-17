package qr

import (
	"bytes"
	"strings"
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

func TestPNGWithLogoEncodesContent(t *testing.T) {
	t.Parallel()
	plain, err := PNG("https://skyl.app/club", 128)
	if err != nil {
		t.Fatal(err)
	}
	withLogo, err := PNGWithLogo("https://skyl.app/club", 128)
	if err != nil {
		t.Fatal(err)
	}
	if len(withLogo) < 8 || !bytes.Equal(withLogo[:8], []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}) {
		t.Fatalf("not a png, len=%d", len(withLogo))
	}
	if bytes.Equal(plain, withLogo) {
		t.Fatal("logo overlay should change the png")
	}
}

func TestLogoFromQuery(t *testing.T) {
	t.Parallel()
	if LogoFromQuery("") || LogoFromQuery("0") {
		t.Fatal("empty")
	}
	if !LogoFromQuery("1") || !LogoFromQuery("true") {
		t.Fatal("true")
	}
}

func TestSessionURL(t *testing.T) {
	t.Parallel()
	id := "11111111-1111-1111-1111-111111111111"
	got := SessionURL(id)
	if !strings.HasSuffix(got, "/v1/sessions/"+id) {
		t.Fatalf("got %s", got)
	}
}
