package media

import "testing"

// SizeObjectKey names a stored size's object as upload wrote it, and
// nothing for a size the image does not have or a key that holds none.
func TestSizeObjectKey(t *testing.T) {
	t.Parallel()
	m := Media{Key: "images/abc.webp", SizeObjects: map[string]SizeObject{
		SizePage: {ImageSize: ImageSize{Width: 1200, Height: 800}, Type: "image/jpeg"},
		SizeCard: {ImageSize: ImageSize{Width: 400, Height: 266}, Type: "image/png"},
	}}
	if key, ctype, ok := m.SizeObjectKey(SizePage); !ok || key != "images/abc.webp/page.jpg" || ctype != "image/jpeg" {
		t.Fatalf("page %q %q %v", key, ctype, ok)
	}
	if key, ctype, ok := m.SizeObjectKey(SizeCard); !ok || key != "images/abc.webp/card.png" || ctype != "image/png" {
		t.Fatalf("card %q %q %v", key, ctype, ok)
	}
	if _, _, ok := (Media{Key: "images/abc.webp"}).SizeObjectKey(SizePage); ok {
		t.Fatal("a size the image has not")
	}
	m.Key = "images/logo.svg"
	if _, _, ok := m.SizeObjectKey(SizePage); ok {
		t.Fatal("a size of an SVG")
	}
}
