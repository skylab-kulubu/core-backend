package media_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"testing"
	"time"

	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

// solidJPEG is a w×h JPEG of one colour, made in memory.
func solidJPEG(t *testing.T, w, h int, c color.Color) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// withJPEGSegment inserts a marker segment right after the JPEG's SOI.
func withJPEGSegment(data []byte, marker byte, payload []byte) []byte {
	size := len(payload) + 2
	segment := append([]byte{0xFF, marker, byte(size >> 8), byte(size)}, payload...)
	out := append([]byte{}, data[:2]...)
	out = append(out, segment...)
	return append(out, data[2:]...)
}

func decodeStored(t *testing.T, data []byte) (image.Image, string) {
	t.Helper()
	img, format, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("stored object does not decode: %v", err)
	}
	return img, format
}

func TestService_PurposeReencodesARasterImageWithoutWhatCameWithIt(t *testing.T) {
	t.Parallel()
	svc, blobs := setup(t)
	photo := solidJPEG(t, 64, 48, color.RGBA{R: 200, G: 30, B: 30, A: 255})
	photo = withJPEGSegment(photo, 0xE1, []byte("Exif\x00\x00GPS-SECRET"))
	photo = withJPEGSegment(photo, 0xFE, []byte("<script>alert(1)</script>"))
	photo = append(photo, []byte("<html>trailing polyglot</html>")...)

	created, err := svc.UploadForPurpose(context.Background(), signedIn("60606060-6060-6060-6060-606060606060"), "profile_picture", uploaded("photo.jpg", "image/jpeg", photo))
	if err != nil {
		t.Fatal(err)
	}
	stored, ok := blobs.Get(created.Key)
	if !ok {
		t.Fatal("no object stored")
	}
	for _, payload := range []string{"GPS-SECRET", "Exif", "<script>", "polyglot"} {
		if bytes.Contains(stored, []byte(payload)) {
			t.Errorf("stored object still carries %q", payload)
		}
	}
	img, format := decodeStored(t, stored)
	if format != "jpeg" || img.Bounds().Dx() != 64 || img.Bounds().Dy() != 48 {
		t.Fatalf("stored %s %v", format, img.Bounds())
	}
	if created.Type != "image/jpeg" || created.Size != int64(len(stored)) {
		t.Fatalf("created %+v, stored %d bytes", created, len(stored))
	}
}

// solid is a w×h image of one colour that encoders read without a pixel
// buffer, so a large test image costs no memory until it is encoded.
type solid struct {
	w, h int
	c    color.Color
}

func (s solid) ColorModel() color.Model { return color.RGBAModel }
func (s solid) Bounds() image.Rectangle { return image.Rect(0, 0, s.w, s.h) }
func (s solid) At(int, int) color.Color { return s.c }

func solidPNG(t *testing.T, w, h int, c color.Color) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := png.Encode(&out, solid{w: w, h: h, c: c}); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestService_PurposeCapsAnImageAtItsMaximumDimension(t *testing.T) {
	t.Parallel()
	svc, blobs := setup(t)
	p := signedIn("61616161-6161-6161-6161-616161616161")

	wide, err := svc.UploadForPurpose(context.Background(), p, "profile_picture", uploaded("wide.png", "image/png", solidPNG(t, 3000, 1000, color.RGBA{G: 128, A: 255})))
	if err != nil {
		t.Fatal(err)
	}
	stored, _ := blobs.Get(wide.Key)
	img, format := decodeStored(t, stored)
	if format != "png" || img.Bounds().Dx() != 2560 || img.Bounds().Dy() != 853 {
		t.Fatalf("stored %s %v, want png 2560×853", format, img.Bounds())
	}
	if wide.Width != 2560 || wide.Height != 853 {
		t.Fatalf("recorded %d×%d, want 2560×853", wide.Width, wide.Height)
	}

	small, err := svc.UploadForPurpose(context.Background(), p, "profile_picture", uploaded("small.png", "image/png", solidPNG(t, 300, 200, color.RGBA{B: 128, A: 255})))
	if err != nil {
		t.Fatal(err)
	}
	if small.Width != 300 || small.Height != 200 {
		t.Fatalf("recorded %d×%d, want 300×200 unchanged", small.Width, small.Height)
	}
}

func TestService_PurposeStoresTheCatalogueSizesOfAnImage(t *testing.T) {
	t.Parallel()
	svc, blobs := setup(t)
	p := signedIn("62626262-6262-6262-6262-626262626262")

	created, err := svc.UploadForPurpose(context.Background(), p, "profile_picture", uploaded("photo.jpg", "image/jpeg", solidJPEG(t, 1600, 1200, color.RGBA{R: 40, G: 90, B: 160, A: 255})))
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]image.Point{"card": {400, 300}, "page": {1200, 900}} {
		key := created.Key + "/" + name
		stored, ok := blobs.Get(key)
		if !ok {
			t.Fatalf("%s: no object at %s", name, key)
		}
		img, format := decodeStored(t, stored)
		if format != "jpeg" || img.Bounds().Size() != want {
			t.Errorf("%s: stored %s %v, want jpeg %v", name, format, img.Bounds().Size(), want)
		}
		if meta, _ := blobs.Metadata(key); meta != (media.BlobMetadata{ContentType: "image/jpeg"}) {
			t.Errorf("%s: metadata %+v", name, meta)
		}
		got := created.Variants[name]
		wantURL := "https://cdn.example.test/" + created.Key + "/" + name
		if got.URL != wantURL || got.Width != want.X || got.Height != want.Y {
			t.Errorf("%s: address %+v, want %s %v", name, got, wantURL, want)
		}
	}
}

func TestService_ASizeAnImageAlreadyFitsIsItsOriginal(t *testing.T) {
	t.Parallel()
	svc, blobs := setup(t)

	created, err := svc.UploadForPurpose(context.Background(), signedIn("63636363-6363-6363-6363-636363636363"), "profile_picture", uploaded("small.jpg", "image/jpeg", solidJPEG(t, 300, 200, color.RGBA{R: 90, A: 255})))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"card", "page"} {
		if _, ok := blobs.Get(created.Key + "/" + name); ok {
			t.Errorf("%s: stored a copy of an image that already fits", name)
		}
		got := created.Variants[name]
		if got.URL != created.URL || got.Width != 300 || got.Height != 200 {
			t.Errorf("%s: address %+v, want the original %s 300×200", name, got, created.URL)
		}
	}
}

// pngClaiming is a PNG whose header claims w×h RGBA pixels, followed by a
// few bytes of image data: a decompression bomb costs its uploader almost
// nothing to send.
func pngClaiming(w, h uint32) []byte {
	chunk := func(kind string, data []byte) []byte {
		out := binary.BigEndian.AppendUint32(nil, uint32(len(data)))
		out = append(out, kind...)
		out = append(out, data...)
		return binary.BigEndian.AppendUint32(out, crc32.ChecksumIEEE(append([]byte(kind), data...)))
	}
	ihdr := binary.BigEndian.AppendUint32(nil, w)
	ihdr = binary.BigEndian.AppendUint32(ihdr, h)
	ihdr = append(ihdr, 8, 6, 0, 0, 0)
	out := []byte("\x89PNG\r\n\x1a\n")
	out = append(out, chunk("IHDR", ihdr)...)
	out = append(out, chunk("IDAT", []byte{0x78, 0x9c, 0x03, 0x00, 0x00, 0x00, 0x00, 0x01})...)
	return append(out, chunk("IEND", nil)...)
}

func TestService_PurposeRefusesAnImageWithTooManyPixelsBeforeDecodingIt(t *testing.T) {
	t.Parallel()
	blobs := media.NewMemoryBlob()
	store := media.NewMemoryStore()
	svc := media.NewService(store, blobs, authz.NewAuthorizer(authz.DefaultPolicy()), "https://cdn.example.test")

	_, err := svc.UploadForPurpose(context.Background(), signedIn("64646464-6464-6464-6464-646464646464"), "profile_picture", uploaded("bomb.png", "image/png", pngClaiming(30000, 30000)))
	var refusal *media.PurposeRefusal
	if !errors.Is(err, media.ErrTooLarge) || !errors.As(err, &refusal) {
		t.Fatalf("err = %v, want %v", err, media.ErrTooLarge)
	}
	if refusal.MaxPixels != media.MaxImagePixels || refusal.Purpose != "profile_picture" {
		t.Fatalf("refusal %+v", refusal)
	}
	if listed, _ := store.List(context.Background()); len(listed) != 0 {
		t.Fatalf("stored %d records", len(listed))
	}
}

func TestService_PurposeRefusesAnImageThatDoesNotDecode(t *testing.T) {
	t.Parallel()
	svc, _ := setup(t)
	broken := append([]byte("\x89PNG\r\n\x1a\n"), []byte("not a png after all")...)

	_, err := svc.UploadForPurpose(context.Background(), signedIn("65656565-6565-6565-6565-656565656565"), "profile_picture", uploaded("broken.png", "image/png", broken))
	var refusal *media.PurposeRefusal
	if !errors.Is(err, media.ErrTypeNotAllowed) || !errors.As(err, &refusal) || refusal.Purpose != "profile_picture" || len(refusal.AllowedTypes) == 0 {
		t.Fatalf("err = %v, want %v with the allowed types", err, media.ErrTypeNotAllowed)
	}
}

// orientationEXIF is an EXIF APP1 payload that holds only the Orientation
// tag (0x0112), big-endian, as a camera writes it.
func orientationEXIF(orientation uint16) []byte {
	out := []byte("Exif\x00\x00MM\x00\x2a\x00\x00\x00\x08")
	out = binary.BigEndian.AppendUint16(out, 1) // one IFD0 entry
	out = binary.BigEndian.AppendUint16(out, 0x0112)
	out = binary.BigEndian.AppendUint16(out, 3) // SHORT
	out = binary.BigEndian.AppendUint32(out, 1)
	out = binary.BigEndian.AppendUint16(out, orientation)
	out = binary.BigEndian.AppendUint16(out, 0)
	return binary.BigEndian.AppendUint32(out, 0) // no next IFD
}

// cornerJPEG is a 64×32 JPEG, blue, with its top-left 32×16 quarter red,
// as a camera stores it before the Orientation tag turns it.
func cornerJPEG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 64, 32))
	for y := 0; y < 32; y++ {
		for x := 0; x < 64; x++ {
			if x < 32 && y < 16 {
				img.Set(x, y, color.RGBA{R: 230, A: 255})
			} else {
				img.Set(x, y, color.RGBA{B: 230, A: 255})
			}
		}
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func isRed(c color.Color) bool {
	r, _, b, _ := c.RGBA()
	return r>>8 > 150 && b>>8 < 100
}

func TestService_PurposeTurnsAPhotoUprightByItsOrientation(t *testing.T) {
	t.Parallel()
	svc, blobs := setup(t)
	p := signedIn("66666666-6666-6666-6666-000000000066")
	for _, tc := range []struct {
		orientation uint16
		size        image.Point
		red, blue   image.Point
	}{
		{orientation: 3, size: image.Pt(64, 32), red: image.Pt(48, 24), blue: image.Pt(16, 8)},
		{orientation: 6, size: image.Pt(32, 64), red: image.Pt(24, 16), blue: image.Pt(8, 48)},
		{orientation: 8, size: image.Pt(32, 64), red: image.Pt(8, 48), blue: image.Pt(24, 16)},
		// The mirrored ones, rare from cameras.
		{orientation: 2, size: image.Pt(64, 32), red: image.Pt(48, 8), blue: image.Pt(16, 8)},
		{orientation: 4, size: image.Pt(64, 32), red: image.Pt(16, 24), blue: image.Pt(16, 8)},
		{orientation: 5, size: image.Pt(32, 64), red: image.Pt(8, 16), blue: image.Pt(24, 48)},
		{orientation: 7, size: image.Pt(32, 64), red: image.Pt(24, 48), blue: image.Pt(8, 16)},
	} {
		photo := withJPEGSegment(cornerJPEG(t), 0xE1, orientationEXIF(tc.orientation))
		created, err := svc.UploadForPurpose(context.Background(), p, "profile_picture", uploaded("phone.jpg", "image/jpeg", photo))
		if err != nil {
			t.Fatalf("orientation %d: %v", tc.orientation, err)
		}
		stored, _ := blobs.Get(created.Key)
		if bytes.Contains(stored, []byte("Exif")) {
			t.Errorf("orientation %d: the stored image keeps an EXIF segment", tc.orientation)
		}
		img, _ := decodeStored(t, stored)
		if img.Bounds().Size() != tc.size || created.Width != tc.size.X || created.Height != tc.size.Y {
			t.Errorf("orientation %d: stored %v, recorded %d×%d, want %v", tc.orientation, img.Bounds().Size(), created.Width, created.Height, tc.size)
			continue
		}
		if !isRed(img.At(tc.red.X, tc.red.Y)) || isRed(img.At(tc.blue.X, tc.blue.Y)) {
			t.Errorf("orientation %d: red corner not where the photo shows it", tc.orientation)
		}
	}
}

// Media uploaded without a purpose keep their bytes apart from the stripped
// metadata. The Orientation tag stays, alone, so a browser still shows a
// phone photo upright.
func TestService_LegacyUploadKeepsOnlyTheOrientationOfAPhoto(t *testing.T) {
	t.Parallel()
	svc, blobs := setup(t)
	p := signedIn("67676767-6767-6767-6767-676767676767")
	exif := append(orientationEXIF(6), []byte("GPS-SECRET")...)

	created, err := svc.Upload(context.Background(), p, "phone.jpg", "image/jpeg", withJPEGSegment(cornerJPEG(t), 0xE1, exif))
	if err != nil {
		t.Fatal(err)
	}
	stored, _ := blobs.Get(created.Key)
	if bytes.Contains(stored, []byte("GPS-SECRET")) {
		t.Fatal("the stored photo keeps the rest of its EXIF")
	}
	onlyOrientation := append([]byte{0xFF, 0xE1, 0x00, 0x22}, orientationEXIF(6)...)
	if !bytes.Contains(stored, onlyOrientation) {
		t.Fatalf("the stored photo lost its orientation: % x", stored[:min(len(stored), 64)])
	}

	upright, err := svc.Upload(context.Background(), p, "upright.jpg", "image/jpeg", withJPEGSegment(cornerJPEG(t), 0xE1, append(orientationEXIF(1), []byte("GPS-SECRET")...)))
	if err != nil {
		t.Fatal(err)
	}
	if stored, _ := blobs.Get(upright.Key); bytes.Contains(stored, []byte("Exif")) {
		t.Fatal("an upright photo keeps an EXIF segment")
	}
}

// One-pixel WebP images from Modernizr's feature tests: a lossy one (opaque
// grey) and one with an alpha channel (fully transparent).
const (
	lossyWebP = "UklGRiIAAABXRUJQVlA4IBYAAAAwAQCdASoBAAEADsD+JaQAA3AAAAAA"
	alphaWebP = "UklGRkoAAABXRUJQVlA4WAoAAAAQAAAAAAAAAAAAQUxQSAwAAAARBxAR/Q9ERP8DAABWUDggGAAAABQBAJ0BKgEAAQAAAP4AAA3AAP7mtQAAAA=="
)

func TestService_PurposeStoresAWebPAsJPEGOrPNGByItsTransparency(t *testing.T) {
	t.Parallel()
	svc, blobs := setup(t)
	p := signedIn("68686868-6868-6868-6868-686868686868")
	for _, tc := range []struct {
		name, webp, wantType, wantFormat string
	}{
		{name: "opaque", webp: lossyWebP, wantType: "image/jpeg", wantFormat: "jpeg"},
		{name: "transparent", webp: alphaWebP, wantType: "image/png", wantFormat: "png"},
	} {
		data, err := base64.StdEncoding.DecodeString(tc.webp)
		if err != nil {
			t.Fatal(err)
		}
		created, err := svc.UploadForPurpose(context.Background(), p, "profile_picture", uploaded("image.webp", "image/webp", data))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		stored, _ := blobs.Get(created.Key)
		img, format := decodeStored(t, stored)
		if created.Type != tc.wantType || format != tc.wantFormat {
			t.Errorf("%s: recorded %s, stored %s; want %s", tc.name, created.Type, format, tc.wantType)
		}
		if meta, _ := blobs.Metadata(created.Key); meta.ContentType != tc.wantType {
			t.Errorf("%s: served as %s", tc.name, meta.ContentType)
		}
		if _, _, _, a := img.At(0, 0).RGBA(); tc.name == "transparent" && a != 0 {
			t.Errorf("transparent: alpha %d after re-encoding", a)
		}
	}
}

func TestService_PurposeKeepsTheFirstFrameOfAnAnimatedGIF(t *testing.T) {
	t.Parallel()
	svc, blobs := setup(t)
	frame := func(c color.Color) *image.Paletted {
		img := image.NewPaletted(image.Rect(0, 0, 8, 8), color.Palette{color.RGBA{R: 230, A: 255}, color.RGBA{B: 230, A: 255}})
		for i := range img.Pix {
			img.Pix[i] = uint8(img.Palette.Index(c))
		}
		return img
	}
	var animated bytes.Buffer
	if err := gif.EncodeAll(&animated, &gif.GIF{
		Image: []*image.Paletted{frame(color.RGBA{R: 230, A: 255}), frame(color.RGBA{B: 230, A: 255})},
		Delay: []int{10, 10},
	}); err != nil {
		t.Fatal(err)
	}

	created, err := svc.UploadForPurpose(context.Background(), signedIn("69696969-6969-6969-6969-696969696969"), "profile_picture", uploaded("wave.gif", "image/gif", animated.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	stored, _ := blobs.Get(created.Key)
	img, format := decodeStored(t, stored)
	if created.Type != "image/png" || format != "png" || !isRed(img.At(4, 4)) {
		t.Fatalf("recorded %s, stored %s, pixel %v; want the red first frame as PNG", created.Type, format, img.At(4, 4))
	}
}

func TestPurgeRemovesTheStoredSizesWithTheImage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := media.NewMemoryStore()
	blobs := media.NewMemoryBlob()
	svc := media.NewService(store, blobs, authz.NewAuthorizer(authz.DefaultPolicy()), "https://cdn.example.test")
	p := signedIn("70707070-7070-7070-7070-707070707070")
	upload := func() media.Media {
		created, err := svc.UploadForPurpose(ctx, p, "profile_picture", uploaded("photo.jpg", "image/jpeg", solidJPEG(t, 1600, 1200, color.RGBA{G: 200, A: 255})))
		if err != nil {
			t.Fatal(err)
		}
		return created
	}
	objects := func(m media.Media) []string {
		var left []string
		for _, key := range []string{m.Key, m.Key + "/card", m.Key + "/page"} {
			if _, ok := blobs.Get(key); ok {
				left = append(left, key)
			}
		}
		return left
	}

	archived, expiring := upload(), upload()
	if len(objects(archived)) != 3 {
		t.Fatalf("stored %v", objects(archived))
	}
	if err := svc.ArchiveOwn(ctx, p, archived.ID); err != nil {
		t.Fatal(err)
	}
	later := time.Now().UTC().Add(31 * 24 * time.Hour)
	if _, err := media.PurgeDeleted(ctx, store, blobs, later, 30*24*time.Hour, 25); err != nil {
		t.Fatal(err)
	}
	if left := objects(archived); len(left) != 0 {
		t.Errorf("archived image purged, objects left: %v", left)
	}
	if _, err := media.PurgeExpired(ctx, store, blobs, later, nil); err != nil {
		t.Fatal(err)
	}
	if left := objects(expiring); len(left) != 0 {
		t.Errorf("expired image purged, objects left: %v", left)
	}
}

func TestService_ARejectedUploadLeavesNoStoredSize(t *testing.T) {
	t.Parallel()
	blobs := media.NewMemoryBlob()
	svc := media.NewService(rejectingCreateStore{MemoryStore: media.NewMemoryStore()}, blobs, authz.NewAuthorizer(authz.DefaultPolicy()), "https://cdn.example.test")

	_, err := svc.UploadForPurpose(context.Background(), signedIn("71717171-7171-7171-7171-717171717171"), "profile_picture", uploaded("photo.jpg", "image/jpeg", solidJPEG(t, 1600, 1200, color.RGBA{R: 10, A: 255})))
	if !errors.Is(err, media.ErrForbidden) {
		t.Fatalf("err = %v", err)
	}
	if left := blobs.Keys(); len(left) != 0 {
		t.Fatalf("rejected upload left objects %v", left)
	}
}

func TestService_LegacyUploadKeepsItsImageAndStoresItsSizes(t *testing.T) {
	t.Parallel()
	svc, blobs := setup(t)
	p := signedIn("72727272-7272-7272-7272-727272727272")
	photo := solidJPEG(t, 1600, 1200, color.RGBA{R: 70, G: 70, B: 200, A: 255})

	created, err := svc.Upload(context.Background(), p, "cover.jpg", "image/jpeg", photo)
	if err != nil {
		t.Fatal(err)
	}
	if original, _ := blobs.Get(created.Key); !bytes.Equal(original, photo) {
		t.Fatal("a legacy image was re-encoded")
	}
	if created.Width != 1600 || created.Height != 1200 {
		t.Fatalf("recorded %d×%d", created.Width, created.Height)
	}
	for name, want := range map[string]image.Point{"card": {400, 300}, "page": {1200, 900}} {
		stored, ok := blobs.Get(created.Key + "/" + name)
		if !ok {
			t.Fatalf("%s not stored", name)
		}
		if img, _ := decodeStored(t, stored); img.Bounds().Size() != want {
			t.Errorf("%s stored %v", name, img.Bounds().Size())
		}
		if got := created.Variants[name]; got.URL != "https://cdn.example.test/"+created.Key+"/"+name {
			t.Errorf("%s address %+v", name, got)
		}
	}

	logo, err := svc.Upload(context.Background(), p, "logo.svg", "image/svg+xml", []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`))
	if err != nil {
		t.Fatal(err)
	}
	if logo.Variants != nil || len(blobs.Keys()) != 3+1 {
		t.Fatalf("an SVG got sizes %v, objects %v", logo.Variants, blobs.Keys())
	}
}
