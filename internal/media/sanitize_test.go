package media

import (
	"bytes"
	"encoding/base64"
	"testing"
)

func TestSanitizeImage_Empty(t *testing.T) {
	t.Parallel()
	_, _, err := sanitizeImage(nil)
	if err == nil {
		t.Fatal("expected invalid")
	}
}

func TestSanitizeImage_RejectsUnknown(t *testing.T) {
	t.Parallel()
	_, _, err := sanitizeImage([]byte("not an image"))
	if err == nil {
		t.Fatal("expected invalid")
	}
}

func TestSanitizeImage_DropsJPEGAPP1(t *testing.T) {
	t.Parallel()
	in := []byte{
		0xFF, 0xD8,
		0xFF, 0xE1, 0x00, 0x06, 0x45, 0x78, 0x00, 0x00,
		0xFF, 0xD9,
	}
	out, ctype, err := sanitizeImage(in)
	if err != nil {
		t.Fatal(err)
	}
	if ctype != "image/jpeg" {
		t.Fatalf("ctype %s", ctype)
	}
	if bytes.Contains(out, []byte{0xFF, 0xE1}) {
		t.Fatalf("APP1 remained %x", out)
	}
	if !bytes.Equal(out, []byte{0xFF, 0xD8, 0xFF, 0xD9}) {
		t.Fatalf("got %x", out)
	}
}

func TestSanitizeImage_KeepsPNGDropsText(t *testing.T) {
	t.Parallel()
	png, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==")
	if err != nil {
		t.Fatal(err)
	}
	out, ctype, err := sanitizeImage(png)
	if err != nil {
		t.Fatal(err)
	}
	if ctype != "image/png" || !bytes.Equal(out[:8], png[:8]) {
		t.Fatalf("ctype %s out %x", ctype, out[:8])
	}

	text := pngWithText(png)
	stripped, _, err := sanitizeImage(text)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stripped, []byte("tEXt")) {
		t.Fatalf("tEXt remained")
	}
	if !bytes.Contains(stripped, []byte("IHDR")) || !bytes.Contains(stripped, []byte("IEND")) {
		t.Fatalf("lost critical chunks")
	}
}

func TestSanitizeImage_SVGStripsScript(t *testing.T) {
	t.Parallel()
	in := []byte(`<svg xmlns="http://www.w3.org/2000/svg" onclick="alert(1)"><script>alert(1)</script><rect width="1" height="1"/></svg>`)
	out, ctype, err := sanitizeImage(in)
	if err != nil {
		t.Fatal(err)
	}
	if ctype != "image/svg+xml" {
		t.Fatalf("ctype %s", ctype)
	}
	s := string(out)
	if bytes.Contains(out, []byte("<script")) || bytes.Contains(out, []byte("onclick")) {
		t.Fatalf("script remained %s", s)
	}
}

func pngWithText(png []byte) []byte {
	iend := bytes.Index(png, []byte("IEND"))
	if iend < 4 {
		return png
	}
	chunkStart := iend - 4
	payload := []byte("Comment\x00hello")
	chunk := make([]byte, 0, 12+len(payload))
	n := len(payload)
	chunk = append(chunk, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
	chunk = append(chunk, "tEXt"...)
	chunk = append(chunk, payload...)
	chunk = append(chunk, 0, 0, 0, 0)
	out := append([]byte{}, png[:chunkStart]...)
	out = append(out, chunk...)
	out = append(out, png[chunkStart:]...)
	return out
}
