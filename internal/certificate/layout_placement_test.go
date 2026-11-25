package certificate

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	"image/jpeg"
	"image/png"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
)

var legacyBackgroundID = uuid.MustParse("6f1c2b52-8f0e-4c55-9f61-2a0d2b7c9e10")

var legacyPreview = PreviewData{RecipientName: "Ada Lovelace", EventName: "SKY LAB Etkinliği", EventDates: "27 Kasım 2026", IssueDate: "27 Kasım 2026", OwnerTeam: "AI", Serial: "SKY-2026-000001"}

// legacyFixture is a layout written before background placement: a 16:9
// image under an A4 landscape page.
func legacyFixture(t *testing.T) (Layout, AssetReader) {
	t.Helper()
	data, err := os.ReadFile("testdata/background-32x18.png")
	if err != nil {
		t.Fatal(err)
	}
	id := legacyBackgroundID
	layout := Layout{
		Width: 1123, Height: 794, Orientation: "landscape", BackgroundColor: "#ffffff", BackgroundMediaID: &id,
		Elements: []Element{
			{ID: "name", Kind: "recipientName", X: 100, Y: 300, Width: 923, Height: 80, FontSize: 44, FontWeight: 700, Align: "center", Fit: "shrink", MinFontSize: 18},
			{ID: "event", Kind: "eventName", X: 100, Y: 400, Width: 923, Height: 50, FontSize: 26, Align: "center"},
			{ID: "team", Kind: "ownerTeam", X: 100, Y: 460, Width: 400, Height: 40, FontSize: 18},
			{ID: "serial", Kind: "serial", X: 100, Y: 700, Width: 400, Height: 30, FontSize: 14},
			{ID: "qr", Kind: "verificationQr", X: 950, Y: 620, Width: 120, Height: 120},
		},
	}
	return layout, renderAssetReader{assets: map[uuid.UUID]Asset{id: {ContentType: "image/png", Data: data}}}
}

type placementCase struct {
	Name      string                          `json:"name"`
	Page      struct{ Width, Height float64 } `json:"page"`
	Source    struct{ Width, Height float64 } `json:"source"`
	Placement BackgroundPlacement             `json:"placement"`
	Want      Rect                            `json:"want"`
}

// The golden table is the geometry's one definition: the panel's editor
// tests the same file (core-frontend src/lib/__fixtures__/
// background-placement.json is a copy of it; change both together).
func placementTable(t *testing.T) (float64, []placementCase) {
	t.Helper()
	raw, err := os.ReadFile("testdata/background-placement.json")
	if err != nil {
		t.Fatal(err)
	}
	var table struct {
		Tolerance float64         `json:"tolerance"`
		Cases     []placementCase `json:"cases"`
	}
	if err := json.Unmarshal(raw, &table); err != nil {
		t.Fatal(err)
	}
	if len(table.Cases) == 0 || table.Tolerance <= 0 {
		t.Fatalf("an empty golden table: %+v", table)
	}
	return table.Tolerance, table.Cases
}

func near(a, b, tolerance float64) bool { return math.Abs(a-b) <= tolerance }

func sameRect(got, want Rect, tolerance float64) bool {
	return near(got.Left, want.Left, tolerance) && near(got.Top, want.Top, tolerance) &&
		near(got.Width, want.Width, tolerance) && near(got.Height, want.Height, tolerance)
}

func TestPlaceBackgroundMatchesTheGoldenTable(t *testing.T) {
	t.Parallel()
	tolerance, cases := placementTable(t)
	fits := map[string]bool{}
	for _, c := range cases {
		fits[c.Placement.Fit] = true
		got := PlaceBackground(c.Page.Width, c.Page.Height, c.Source.Width, c.Source.Height, c.Placement)
		if !sameRect(got, c.Want, tolerance) {
			t.Errorf("%s: got %+v, want %+v", c.Name, got, c.Want)
		}
	}
	for _, fit := range []string{FitContain, FitCover, FitFill, FitWidth, FitHeight, FitNone} {
		if !fits[fit] {
			t.Errorf("the golden table has no %s case", fit)
		}
	}
}

// pdfcpu v0.15.0 (Gotenberg 8.37) scales a "scale:1" relative watermark
// by the source's longer side: a landscape (or square) PDF to the page's
// width, a portrait one to its height, centred.
func TestLegacyPDFPlacementFollowsPdfcpu(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		w, h float64
		want string
	}{{842, 595, FitWidth}, {595, 842, FitHeight}, {600, 600, FitWidth}} {
		if got := LegacyPDFPlacement(c.w, c.h); got != (BackgroundPlacement{Fit: c.want, X: 0.5, Y: 0.5, Scale: 1}) {
			t.Errorf("%gx%g: %+v, want %s centred", c.w, c.h, got, c.want)
		}
	}
	// A portrait A4 PDF under an A4 landscape page: its height fills the
	// page, centred, with margins left and right.
	page := PlaceBackground(1123, 794, 595*96/72.0, 842*96/72.0, LegacyPDFPlacement(595, 842))
	if !sameRect(page, Rect{Left: 280.9596, Top: 0, Width: 561.0808, Height: 794}, 0.01) {
		t.Fatalf("portrait PDF on a landscape page: %+v", page)
	}
	if LegacyImagePlacement != (BackgroundPlacement{Fit: FitCover, X: 0.5, Y: 0.5, Scale: 1}) {
		t.Fatalf("legacy image placement: %+v", LegacyImagePlacement)
	}
}

func TestValidateLayoutChecksTheBackgroundPlacement(t *testing.T) {
	t.Parallel()
	base, _ := legacyFixture(t)
	for name, c := range map[string]struct {
		placement BackgroundPlacement
		noMedia   bool
		ok        bool
	}{
		"contain":         {placement: BackgroundPlacement{Fit: FitContain, X: 0.5, Y: 0.5, Scale: 1}, ok: true},
		"limits":          {placement: BackgroundPlacement{Fit: FitNone, X: 0, Y: 1, Scale: 0.1}, ok: true},
		"upper scale":     {placement: BackgroundPlacement{Fit: FitWidth, X: 1, Y: 0, Scale: 10}, ok: true},
		"fill":            {placement: BackgroundPlacement{Fit: FitFill, X: 0.5, Y: 0.5, Scale: 1}, ok: true},
		"no background":   {placement: BackgroundPlacement{Fit: FitContain, X: 0.5, Y: 0.5, Scale: 1}, noMedia: true},
		"unknown fit":     {placement: BackgroundPlacement{Fit: "scale-down", X: 0.5, Y: 0.5, Scale: 1}},
		"empty fit":       {placement: BackgroundPlacement{X: 0.5, Y: 0.5, Scale: 1}},
		"x below 0":       {placement: BackgroundPlacement{Fit: FitCover, X: -0.01, Y: 0.5, Scale: 1}},
		"y over 1":        {placement: BackgroundPlacement{Fit: FitCover, X: 0.5, Y: 1.01, Scale: 1}},
		"scale under 0.1": {placement: BackgroundPlacement{Fit: FitCover, X: 0.5, Y: 0.5, Scale: 0.09}},
		"scale over 10":   {placement: BackgroundPlacement{Fit: FitCover, X: 0.5, Y: 0.5, Scale: 10.01}},
		"scale left out":  {placement: BackgroundPlacement{Fit: FitCover, X: 0.5, Y: 0.5}},
		"NaN position":    {placement: BackgroundPlacement{Fit: FitCover, X: math.NaN(), Y: 0.5, Scale: 1}},
		"infinite scale":  {placement: BackgroundPlacement{Fit: FitCover, X: 0.5, Y: 0.5, Scale: math.Inf(1)}},
	} {
		layout := base
		placement := c.placement
		layout.BackgroundPlacement = &placement
		if c.noMedia {
			layout.BackgroundMediaID = nil
		}
		err := ValidateLayout(layout)
		var refused *InvalidError
		switch {
		case c.ok && err != nil:
			t.Errorf("%s: refused: %v", name, err)
		case !c.ok && (!errors.As(err, &refused) || refused.Code != CodeLayoutPlacementInvalid):
			t.Errorf("%s: %v, want %s", name, err, CodeLayoutPlacementInvalid)
		}
	}
}

// layoutBefore is Layout as it was before background placement: a layout
// without one is stored byte for byte as before, so its checksum stays.
type layoutBefore struct {
	Width             float64    `json:"width"`
	Height            float64    `json:"height"`
	Orientation       string     `json:"orientation"`
	BackgroundColor   string     `json:"backgroundColor"`
	BackgroundMediaID *uuid.UUID `json:"backgroundMediaId,omitempty"`
	Elements          []Element  `json:"elements"`
}

func TestLegacyLayoutIsStoredAndPrintedAsBefore(t *testing.T) {
	t.Parallel()
	layout, assets := legacyFixture(t)
	stored, err := marshalLayout(layout)
	if err != nil {
		t.Fatal(err)
	}
	before, err := json.Marshal(layoutBefore{layout.Width, layout.Height, layout.Orientation, layout.BackgroundColor, layout.BackgroundMediaID, layout.Elements})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored, before) {
		t.Fatalf("a layout without a placement is stored differently:\n%s\n%s", stored, before)
	}
	sum, err := LayoutChecksum(layout)
	if err != nil {
		t.Fatal(err)
	}
	if want := sha256.Sum256(before); sum != hex.EncodeToString(want[:]) {
		t.Fatalf("checksum changed: %s", sum)
	}

	// testdata/legacy-layout.html was printed by core before placement
	// (origin/main 77b2211).
	golden, err := os.ReadFile("testdata/legacy-layout.html")
	if err != nil {
		t.Fatal(err)
	}
	html, err := LayoutHTML(t.Context(), layout, legacyPreview, "https://skyl.app/c/LEGACY", assets)
	if err != nil {
		t.Fatal(err)
	}
	if html != string(golden) {
		t.Fatalf("a layout without a placement prints differently:\n%s", html)
	}
}

var placedImage = regexp.MustCompile(`<main class="page"><img alt="" aria-hidden="true" style="position:absolute;left:([-0-9.e+]+)px;top:([-0-9.e+]+)px;width:([0-9.e+]+)px;height:([0-9.e+]+)px" src="data:image/png;base64,`)

// The background's <img> takes the golden table's box, under every
// element, and the page has no CSS background image left.
func TestLayoutHTMLPlacesTheImageBackground(t *testing.T) {
	t.Parallel()
	tolerance, cases := placementTable(t)
	sources := map[[2]float64][]byte{}
	for _, c := range cases {
		key := [2]float64{c.Source.Width, c.Source.Height}
		if _, ok := sources[key]; ok {
			continue
		}
		var buf bytes.Buffer
		if err := png.Encode(&buf, image.NewGray(image.Rect(0, 0, int(key[0]), int(key[1])))); err != nil {
			t.Fatal(err)
		}
		sources[key] = buf.Bytes()
	}
	id := uuid.New()
	for _, c := range cases {
		assets := renderAssetReader{assets: map[uuid.UUID]Asset{id: {ContentType: "image/png", Data: sources[[2]float64{c.Source.Width, c.Source.Height}]}}}
		placement := c.Placement
		orientation := "landscape"
		if c.Page.Height > c.Page.Width {
			orientation = "portrait"
		}
		layout := Layout{Width: c.Page.Width, Height: c.Page.Height, Orientation: orientation, BackgroundMediaID: &id, BackgroundPlacement: &placement,
			Elements: []Element{{ID: "name", Kind: "recipientName", X: 10, Y: 10, Width: 400, Height: 40}, {ID: "serial", Kind: "serial", X: 10, Y: 60, Width: 400, Height: 40}}}
		html, err := LayoutHTML(t.Context(), layout, PreviewData{RecipientName: "Ada", Serial: "SKY-1"}, "https://skyl.app/c/X", assets)
		if err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		m := placedImage.FindStringSubmatch(html)
		if m == nil {
			t.Fatalf("%s: no placed background first on the page: %s", c.Name, html)
		}
		var got [4]float64
		for i := range got {
			if got[i], err = strconv.ParseFloat(m[i+1], 64); err != nil {
				t.Fatal(err)
			}
		}
		if !sameRect(Rect{got[0], got[1], got[2], got[3]}, c.Want, tolerance) {
			t.Errorf("%s: <img> box %v, want %+v", c.Name, got, c.Want)
		}
		if strings.Contains(html, "background-image") {
			t.Fatalf("%s: a placed background is also a CSS background", c.Name)
		}
	}
}

func TestBackgroundSize(t *testing.T) {
	t.Parallel()
	encode := func(f func(*bytes.Buffer) error) []byte {
		var buf bytes.Buffer
		if err := f(&buf); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	pngData := encode(func(b *bytes.Buffer) error { return png.Encode(b, image.NewGray(image.Rect(0, 0, 40, 20))) })
	jpegData := encode(func(b *bytes.Buffer) error { return jpeg.Encode(b, image.NewGray(image.Rect(0, 0, 40, 20)), nil) })
	// The same JPEG with EXIF Orientation 6 (turned a quarter): a browser
	// draws it 20 wide and 40 tall.
	exif := []byte("Exif\x00\x00II*\x00\x08\x00\x00\x00\x01\x00\x12\x01\x03\x00\x01\x00\x00\x00\x06\x00\x00\x00\x00\x00\x00\x00")
	app1 := append([]byte{0xFF, 0xE1, 0, byte(len(exif) + 2)}, exif...)
	turned := append(append(append([]byte(nil), jpegData[:2]...), app1...), jpegData[2:]...)
	// A lossless WebP header (VP8L), 40×30.
	bits := uint32(39) | uint32(29)<<14
	vp8l := []byte{0x2f, byte(bits), byte(bits >> 8), byte(bits >> 16), byte(bits >> 24), 0}
	webp := append([]byte("RIFF\x12\x00\x00\x00WEBPVP8L\x05\x00\x00\x00"), vp8l...)

	for name, c := range map[string]struct {
		contentType string
		data        []byte
		w, h        float64
	}{
		"png":              {"image/png", pngData, 40, 20},
		"jpeg":             {"image/jpeg", jpegData, 40, 20},
		"turned jpeg":      {"image/jpeg", turned, 20, 40},
		"webp":             {"image/webp", webp, 40, 30},
		"svg size":         {"image/svg+xml", []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg" width="800px" height="600" viewBox="0 0 10 10"/>`), 800, 600},
		"svg viewBox":      {"image/svg+xml", []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 1920 1080"/>`), 1920, 1080},
		"svg width":        {"image/svg+xml; charset=utf-8", []byte(`<svg width="960" height="100%" viewBox="0,0,1920,1080"/>`), 960, 540},
		"svg no size":      {"image/svg+xml", []byte(`<svg width="100%"/>`), 0, 0},
		"svg in mm":        {"image/svg+xml", []byte(`<svg width="297mm" height="210mm"/>`), 297 * 96 / 25.4, 210 * 96 / 25.4},
		"svg in pt and in": {"image/svg+xml", []byte(`<svg width="842pt" height="8.5in"/>`), 842 * 96 / 72.0, 8.5 * 96},
		"svg in cm and pc": {"image/svg+xml", []byte(`<svg width="10cm" height="6pc"/>`), 10 * 96 / 2.54, 6 * 16},
		"svg spaced unit":  {"image/svg+xml", []byte(`<svg width="10 cm" height="6pc" viewBox="0 0 200 100"/>`), 12 * 16, 6 * 16},
		"svg spaced only":  {"image/svg+xml", []byte(`<svg width="10 cm" height="5 mm"/>`), 0, 0},
		"svg hex and dot":  {"image/svg+xml", []byte(`<svg width="0x1p4" height="5." viewBox="0 0 30 20"/>`), 30, 20},
		"svg exponent":     {"image/svg+xml", []byte(`<svg width="1.5e2" height=" 50 "/>`), 150, 50},
		"svg too large":    {"image/svg+xml", []byte(`<svg width="2e6" height="10"/>`), 0, 0},
		"svg huge viewBox": {"image/svg+xml", []byte(`<svg viewBox="0 0 1e308 1e308"/>`), 0, 0},
		"svg in em":        {"image/svg+xml", []byte(`<svg width="10em" height="5em" viewBox="0 0 200 100"/>`), 200, 100},
		"svg tiny":         {"image/svg+xml", []byte(`<svg width="0.0000001" height="0.0000001"/>`), 0, 0},
		"svg tiny viewBox": {"image/svg+xml", []byte(`<svg viewBox="0 0 1e-9 1e-9"/>`), 0, 0},
		"svg negative":     {"image/svg+xml", []byte(`<svg width="-10" height="10"/>`), 0, 0},
		"not an svg":       {"image/svg+xml", []byte(`<html/>`), 0, 0},
		"unreadable image": {"image/png", []byte("not an image"), 0, 0},
	} {
		w, h, err := backgroundSize(c.contentType, c.data)
		if c.w == 0 {
			var refused *InvalidError
			if !errors.As(err, &refused) || refused.Code != CodeBackgroundSizeUnknown {
				t.Errorf("%s: %v, want %s", name, err, CodeBackgroundSizeUnknown)
			}
			continue
		}
		if err != nil || !near(w, c.w, 1e-9) || !near(h, c.h, 1e-9) {
			t.Errorf("%s: %gx%g %v, want %gx%g", name, w, h, err, c.w, c.h)
		}
	}

	layout, _ := legacyFixture(t)
	layout.BackgroundPlacement = &BackgroundPlacement{Fit: FitContain, X: 0.5, Y: 0.5, Scale: 1}
	assets := renderAssetReader{assets: map[uuid.UUID]Asset{*layout.BackgroundMediaID: {ContentType: "image/png", Data: []byte("not an image")}}}
	_, err := LayoutHTML(t.Context(), layout, legacyPreview, "https://skyl.app/c/X", assets)
	var refused *InvalidError
	if !errors.As(err, &refused) || refused.Code != CodeBackgroundSizeUnknown {
		t.Fatalf("unreadable placed background: %v", err)
	}
}

// A PDF background is printed as before until pdf-tools places it, also
// when the layout has a placement.
func TestPlacedPDFBackgroundIsPrintedAsBefore(t *testing.T) {
	t.Parallel()
	id := uuid.New()
	background := []byte("%PDF-1.7 vector certificate")
	assets := renderAssetReader{assets: map[uuid.UUID]Asset{id: {ContentType: "application/pdf", Data: background}}}
	renderer := &backgroundRecorder{}
	layout, _ := legacyFixture(t)
	layout.BackgroundMediaID = &id
	layout.BackgroundPlacement = &BackgroundPlacement{Fit: FitContain, X: 0, Y: 0, Scale: 1}
	pdf, err := (&service{render: renderer}).renderLayoutPDF(t.Context(), layout, legacyPreview, "https://skyl.app/c/X", assets)
	if err != nil {
		t.Fatal(err)
	}
	if string(pdf) != "%PDF-composed" || !bytes.Equal(renderer.background, background) || strings.Contains(renderer.html, `aria-hidden="true" style="position:absolute;left:`) {
		t.Fatalf("pdf=%q background=%q html=%s", pdf, renderer.background, renderer.html)
	}
}
