package ticketpage_test

import (
	"bytes"
	"context"
	"encoding/base64"
	stdhtml "html"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"regexp"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/ticketimage"
	"github.com/skylab-kulubu/core-backend/internal/ticketpage"
)

// halves is a picture red left of 40 % of its width and blue right of it,
// as a PNG: its middle is blue.
func halves(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			c := color.RGBA{220, 20, 20, 255}
			if x >= w*2/5 {
				c = color.RGBA{20, 20, 220, 255}
			}
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// mostlyBlue reports whether a JPEG band is blue in its middle.
func mostlyBlue(t *testing.T, band []byte) bool {
	t.Helper()
	img, err := jpeg.Decode(bytes.NewReader(band))
	if err != nil {
		t.Fatal(err)
	}
	b := img.Bounds()
	r, _, bl, _ := img.At(b.Min.X+b.Dx()/2, b.Min.Y+b.Dy()/2).RGBA()
	return bl > r
}

// A region is cut where it lies and narrowed to the band's shape; the band
// is never wider than the region's pixels nor than 300 dpi's 2150.
func TestBandCutsTheRegion(t *testing.T) {
	t.Parallel()
	data := halves(t, 2400, 1200)
	right := &ticketimage.Crop{X: 0.5, Y: 0.25, W: 0.5, H: 0.1}
	band, ok := ticketpage.Band(data, right)
	if !ok {
		t.Fatal("no band")
	}
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(band))
	if err != nil {
		t.Fatal(err)
	}
	// 0.5 × 2400 = 1200 wide, 0.1 × 1200 = 120 high: narrower than the band's
	// shape, so its 120 pixels set the height and 910 the width.
	if cfg.Width != 910 || cfg.Height != 120 {
		t.Fatalf("band %dx%d", cfg.Width, cfg.Height)
	}
	if !mostlyBlue(t, band) {
		t.Error("the right half's region is not blue")
	}
	left, ok := ticketpage.Band(data, &ticketimage.Crop{X: 0, Y: 0, W: 0.5, H: 1})
	if !ok || mostlyBlue(t, left) {
		t.Error("the left half's region is not red")
	}
	whole, _ := ticketpage.Band(data, nil)
	if cfg, _ := jpeg.DecodeConfig(bytes.NewReader(whole)); cfg.Width != 2150 || cfg.Height != 284 {
		t.Errorf("whole band %dx%d", cfg.Width, cfg.Height)
	}
	for _, bad := range []ticketimage.Crop{{X: -0.1, Y: 0, W: 0.5, H: 0.5}, {X: 0.8, Y: 0, W: 0.5, H: 0.5}, {X: 0, Y: 0, W: 0.001, H: 0.5}} {
		if _, ok := ticketpage.Band(data, &bad); ok {
			t.Errorf("a band of region %+v", bad)
		}
	}
}

var bandData = regexp.MustCompile(`data:image/jpeg;base64,([A-Za-z0-9+/=]+)`)

// bandOf is the band's JPEG the HTML embeds (the fixture's ticket has no
// map, the band's only JPEG neighbour).
func bandOf(t *testing.T, html string) []byte {
	t.Helper()
	m := bandData.FindStringSubmatch(stdhtml.UnescapeString(html))
	if m == nil {
		t.Fatal("no band in the HTML")
	}
	data, err := base64.StdEncoding.DecodeString(m[1])
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// fakeTicketImages answers one Event's choice.
type fakeTicketImages struct {
	setting ticketimage.Setting
}

func (f fakeTicketImages) Get(context.Context, uuid.UUID) (ticketimage.Setting, error) {
	return f.setting, nil
}

// bandImages answers images by id, with BandSource (the original) for
// those in originals.
type bandImages struct {
	pages     map[uuid.UUID][]byte
	originals map[uuid.UUID][]byte
	asked     []uuid.UUID
}

func (b *bandImages) Image(_ context.Context, id uuid.UUID, _ string) ([]byte, string, bool) {
	b.asked = append(b.asked, id)
	data, ok := b.pages[id]
	return data, "image/png", ok
}

func (b *bandImages) BandSource(ctx context.Context, id uuid.UUID) ([]byte, bool) {
	if data, ok := b.originals[id]; ok {
		b.asked = append(b.asked, id)
		return data, true
	}
	data, _, ok := b.Image(ctx, id, "page")
	return data, ok
}

// The PDF's band follows the Event's choice: an uploaded image's region,
// the cover's region while the cover is the one it was drawn on, else the
// cover's middle; an image that cannot be read gives the cover's middle.
func TestPDFBandFollowsTheTicketImage(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()
	cover, logo, gone := uuid.New(), uuid.New(), uuid.New()
	ev := f.ev
	ev.CoverImageID = &cover
	if _, err := f.events.Update(ctx, ev); err != nil {
		t.Fatal(err)
	}
	images := &bandImages{
		pages:     map[uuid.UUID][]byte{cover: halves(t, 1200, 600)},
		originals: map[uuid.UUID][]byte{logo: halves(t, 2000, 1000)},
	}
	g := f.guest("elif@example.com")
	right := &ticketimage.Crop{X: 0.5, Y: 0, W: 0.5, H: 0.2}
	for _, tc := range []struct {
		name    string
		setting ticketimage.Setting
		asked   uuid.UUID
		blue    bool
	}{
		{"auto", ticketimage.Setting{Source: ticketimage.SourceAuto}, cover, true},
		{"cover region", ticketimage.Setting{Source: ticketimage.SourceCover, CoverMediaID: &cover, Crop: &ticketimage.Crop{X: 0, Y: 0, W: 0.5, H: 0.2}}, cover, false},
		{"an old cover's region", ticketimage.Setting{Source: ticketimage.SourceCover, CoverMediaID: &gone, Crop: &ticketimage.Crop{X: 0, Y: 0, W: 0.5, H: 0.2}}, cover, true},
		{"upload", ticketimage.Setting{Source: ticketimage.SourceUpload, MediaID: &logo, Crop: right}, logo, true},
		{"unreadable upload", ticketimage.Setting{Source: ticketimage.SourceUpload, MediaID: &gone, Crop: &ticketimage.Crop{X: 0, Y: 0, W: 0.5, H: 0.2}}, cover, true},
	} {
		images.asked = nil
		svc := ticketpage.New(f.store, f.events, f.users, f.signer, f.render, images, ticketpage.Config{
			Page: ticket.PageConfig{Grace: ticket.DefaultPageGrace, URL: "https://yildizskylab.com/bilet/{key}"},
		})
		svc.Now = func() time.Time { return f.now }
		svc.TicketImages = fakeTicketImages{setting: tc.setting}
		if _, _, err := svc.PDF(ctx, g.AccessKey); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(images.asked) == 0 || images.asked[len(images.asked)-1] != tc.asked {
			t.Errorf("%s: asked %v", tc.name, images.asked)
		}
		band := bandOf(t, f.render.html)
		if mostlyBlue(t, band) != tc.blue {
			t.Errorf("%s: band blue = %v", tc.name, !tc.blue)
		}
	}
}
