package certificate

import (
	"fmt"
	"math"
	"strings"
	"testing"
)

// A4 landscape PDF drawn over a page of its own size (1 pt = 4/3 px).
const (
	testPDFW = 842.0
	testPDFH = 595.0
)

var testBox = Rect{Width: testPDFW * pxPerPt, Height: testPDFH * pxPerPt}

// textSpan is a span whose baseline starts at (x, y) and runs width pt in
// the direction angle (degrees, clockwise as shown).
func textSpan(id, text string, x, y, size, angle, width float64, font PDFFont) PDFSpan {
	theta := angle * math.Pi / 180
	ux, uy := math.Cos(theta), math.Sin(theta)
	vx, vy := -uy, ux
	bl := [2]float64{x + vx*0.2*size, y + vy*0.2*size}
	br := [2]float64{bl[0] + ux*width, bl[1] + uy*width}
	tr := [2]float64{br[0] - vx*size, br[1] - vy*size}
	tl := [2]float64{bl[0] - vx*size, bl[1] - vy*size}
	if font.Ascent == 0 {
		font.Ascent, font.Descent = 0.8, -0.2
	}
	span := PDFSpan{ID: id, Text: text, Quad: [][2]float64{bl, br, tr, tl}, Angle: angle, Size: size, Font: font,
		Color: "#1a2659", Opacity: 1, Convertible: true}
	span.Origin.X, span.Origin.Y = x, y
	return span
}

var (
	montserratBold = PDFFont{Name: "Montserrat-Bold", Weight: 700}
	helvetica      = PDFFont{Name: "Helvetica", Weight: 400}
	helveticaBold  = PDFFont{Name: "Helvetica-Bold", Weight: 700}
)

// pdfText is a page of the given spans, each its own run on its own line
// in its own block unless lines says otherwise (span id → line, block).
func pdfText(spans []PDFSpan, lines map[string][2]int) PDFText {
	text := PDFText{Info: PDFInfo{Pages: 1, WidthPt: testPDFW, HeightPt: testPDFH, TextCount: len(spans)}, Spans: spans}
	for n, span := range spans {
		line, block := n, n
		if lb, ok := lines[span.ID]; ok {
			line, block = lb[0], lb[1]
		}
		text.Runs = append(text.Runs, PDFRun{SpanIDs: []string{span.ID}, Text: span.Text, Line: line, Block: block})
	}
	return text
}

func sequentialIDs() func() string {
	n := 0
	return func() string {
		n++
		return fmt.Sprintf("bgtext-%d", n)
	}
}

func nearPx(a, b float64) bool { return math.Abs(a-b) < 0.02 }

func centre(el Element) (float64, float64) { return el.X + el.Width/2, el.Y + el.Height/2 }

func convert(t *testing.T, text PDFText, chosen []string, kinds map[string]string) ([]Element, []TextWarning) {
	t.Helper()
	elements, warnings := textElements(text, chosen, kinds, testBox.Width, testBox.Height, testBox, nil, sequentialIDs())
	for _, el := range elements {
		layout := Layout{Width: testBox.Width, Height: testBox.Height, Orientation: "landscape", Elements: []Element{
			el, {ID: "name", Kind: "recipientName", X: 1, Y: 1, Width: 10, Height: 10}, {ID: "serial", Kind: "serial", X: 1, Y: 20, Width: 10, Height: 10},
		}}
		if err := ValidateLayout(layout); err != nil {
			t.Fatalf("element %+v: %v", el, err)
		}
	}
	return elements, warnings
}

// One line: its box grows 15% from the edge it keeps, its line box is
// centred on the font's middle, it shrinks to fit, and it keeps the PDF's
// font name, colour and weight.
func TestTextElementsOneLine(t *testing.T) {
	t.Parallel()
	text := pdfText([]PDFSpan{textSpan("4", "Katılım Belgesi", 100, 200, 30, 0, 300, montserratBold)}, nil)
	elements, warnings := convert(t, text, []string{"4"}, nil)
	if len(elements) != 1 || len(warnings) != 0 {
		t.Fatalf("elements %+v, warnings %+v", elements, warnings)
	}
	el := elements[0]
	// mid = 200 − (0.8 − 0.2)/2 · 30 = 191; box 36 pt tall around it;
	// 300 pt wide, grown to 345 to the right.
	if !nearPx(el.X, 100*pxPerPt) || !nearPx(el.Y, 173*pxPerPt) || !nearPx(el.Width, 345*pxPerPt) || !nearPx(el.Height, 36*pxPerPt) {
		t.Fatalf("box %+v", el)
	}
	if el.Kind != "staticText" || el.Text != "Katılım Belgesi" || el.FontFamily != "Montserrat" || el.FontWeight != 700 ||
		el.FontSize != 40 || el.MinFontSize != 34 || el.Fit != "shrink" || el.Align != "left" || el.LineHeight != textLineHeight ||
		el.Color != "#1a2659" || el.Opacity != nil || el.Rotation != 0 || el.Origin != OriginBackgroundText ||
		el.SourceFont != "Montserrat-Bold" || el.FontStyle != "" || el.ID != "bgtext-1" {
		t.Fatalf("element %+v", el)
	}
}

// A line whose middle is within 2% of the page's middle is centred and
// grows on both sides; one elsewhere is left aligned.
func TestTextElementsCentredLine(t *testing.T) {
	t.Parallel()
	text := pdfText([]PDFSpan{textSpan("1", "Başarı Belgesi", 421-150+5, 200, 30, 0, 300, montserratBold)}, nil)
	elements, _ := convert(t, text, []string{"1"}, nil)
	el := elements[0]
	cx, _ := centre(el)
	if el.Align != "center" || !nearPx(cx, (421+5)*pxPerPt) || !nearPx(el.Width, 345*pxPerPt) {
		t.Fatalf("centred %+v", el)
	}
}

// Turkish: decomposed letters come together (NFC); every Turkish letter
// stays.
func TestTextElementsTurkish(t *testing.T) {
	t.Parallel()
	decomposed := "Ayşe Yıldız ğĞıİşŞçÇöÖüÜ"
	text := pdfText([]PDFSpan{textSpan("2", decomposed, 100, 300, 20, 0, 260, helvetica)}, nil)
	elements, _ := convert(t, text, []string{"2"}, nil)
	if got := elements[0].Text; got != "Ayşe Yıldız ğĞıİşŞçÇöÖüÜ" || strings.ContainsRune(got, '̧') {
		t.Fatalf("text %q", got)
	}
}

// A turned text keeps its angle; the element turns about its centre,
// which is the text's middle (grown along the text's own direction).
func TestTextElementsRotated(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		angle float64
	}{{-90}, {-30}, {90}} {
		t.Run(fmt.Sprint(tc.angle), func(t *testing.T) {
			t.Parallel()
			text := pdfText([]PDFSpan{textSpan("9", "İmza", 200, 400, 20, tc.angle, 100, helvetica)}, nil)
			elements, _ := convert(t, text, []string{"9"}, nil)
			el := elements[0]
			if el.Rotation != tc.angle || el.Align != "left" {
				t.Fatalf("element %+v", el)
			}
			theta := tc.angle * math.Pi / 180
			ux, uy, vx, vy := math.Cos(theta), math.Sin(theta), -math.Sin(theta), math.Cos(theta)
			// Along: 0 … 115 from the origin; across: the middle, 0.3 · 20
			// above the baseline.
			wantX := 200 + ux*57.5 - vx*6
			wantY := 400 + uy*57.5 - vy*6
			cx, cy := centre(el)
			if !nearPx(cx, wantX*pxPerPt) || !nearPx(cy, wantY*pxPerPt) || !nearPx(el.Width, 115*pxPerPt) || !nearPx(el.Height, 24*pxPerPt) {
				t.Fatalf("centre (%v, %v), want (%v, %v); %+v", cx, cy, wantX*pxPerPt, wantY*pxPerPt, el)
			}
		})
	}
}

// Lines of one style, evenly spaced and sharing an edge, are one
// paragraph: its alignment that edge, its line height their spacing.
func TestTextElementsParagraphs(t *testing.T) {
	t.Parallel()
	widths := []float64{126, 77, 84}
	for _, align := range []string{"left", "center", "right"} {
		t.Run(align, func(t *testing.T) {
			t.Parallel()
			var spans []PDFSpan
			lines := map[string][2]int{}
			for i, w := range widths {
				x := 60.0
				switch align {
				case "center":
					x = 600 - w/2
				case "right":
					x = 780 - w
				}
				id := fmt.Sprint(10 + i)
				spans = append(spans, textSpan(id, fmt.Sprintf("satır %d", i+1), x, 300+float64(i)*18, 14, 0, w, helvetica))
				lines[id] = [2]int{i, 7}
			}
			elements, _ := convert(t, pdfText(spans, lines), []string{"10", "11", "12"}, nil)
			if len(elements) != 1 {
				t.Fatalf("elements %+v", elements)
			}
			el := elements[0]
			if el.Align != align || el.Text != "satır 1\nsatır 2\nsatır 3" || el.Fit != "" || el.MinFontSize != 0 ||
				!nearPx(el.LineHeight, round2(18.0/14)) || !nearPx(el.Height, 54*pxPerPt) {
				t.Fatalf("paragraph %+v", el)
			}
		})
	}
}

// A paragraph's lines that are not chosen split it: the chosen lines
// around an unchosen one are not joined across it.
func TestTextElementsParagraphGap(t *testing.T) {
	t.Parallel()
	var spans []PDFSpan
	lines := map[string][2]int{}
	for i := range 3 {
		id := fmt.Sprint(20 + i)
		spans = append(spans, textSpan(id, fmt.Sprintf("satır %d", i+1), 60, 300+float64(i)*18, 14, 0, 100, helvetica))
		lines[id] = [2]int{i, 3}
	}
	elements, _ := convert(t, pdfText(spans, lines), []string{"20", "22"}, nil)
	if len(elements) != 2 || elements[0].Text != "satır 1" || elements[1].Text != "satır 3" {
		t.Fatalf("elements %+v", elements)
	}
}

// A line of mixed styles keeps each style as its own element with its own
// box; none of them joins a paragraph.
func TestTextElementsMixedLine(t *testing.T) {
	t.Parallel()
	spans := []PDFSpan{
		textSpan("30", "Bu belge", 60, 400, 18, 0, 70, helvetica),
		textSpan("31", "Ahmet Y", 135, 400, 18, 0, 70, helveticaBold),
		textSpan("32", "adına", 210, 400, 18, 0, 50, helvetica),
	}
	spans[1].Color = "#cc1a1a"
	lines := map[string][2]int{"30": {0, 0}, "31": {0, 0}, "32": {0, 0}}
	elements, _ := convert(t, pdfText(spans, lines), []string{"30", "31", "32"}, nil)
	if len(elements) != 3 {
		t.Fatalf("elements %+v", elements)
	}
	for i, want := range []struct {
		text, family string
		weight       int
		color        string
		x            float64
	}{{"Bu belge", "Helvetica", 400, "#1a2659", 60}, {"Ahmet Y", "Helvetica", 700, "#cc1a1a", 135}, {"adına", "Helvetica", 400, "#1a2659", 210}} {
		el := elements[i]
		if el.Text != want.text || el.FontFamily != want.family || el.FontWeight != want.weight || el.Color != want.color || !nearPx(el.X, want.x*pxPerPt) || el.Align != "left" {
			t.Fatalf("element %d %+v", i, el)
		}
	}
}

// Colour (pdf-tools gives sRGB, CMYK converted) and opacity: a colour that
// is not #rrggbb is black; an opacity under 1 is kept.
func TestTextElementsColourAndOpacity(t *testing.T) {
	t.Parallel()
	cmyk := textSpan("40", "Yarı saydam", 100, 200, 20, 0, 120, helvetica)
	cmyk.Color, cmyk.Opacity = "#00A3E0", 0.5
	broken := textSpan("41", "Renk yok", 100, 260, 20, 0, 120, helvetica)
	broken.Color = "cmyk(1,0,0,0)"
	elements, _ := convert(t, pdfText([]PDFSpan{cmyk, broken}, nil), []string{"40", "41"}, nil)
	if elements[0].Color != "#00a3e0" || elements[0].Opacity == nil || *elements[0].Opacity != 0.5 || elements[1].Color != "#000000" {
		t.Fatalf("elements %+v / %+v", elements[0], elements[1])
	}
}

// kinds turns a piece into a field: its box and style stay, its text is
// not written.
func TestTextElementsKinds(t *testing.T) {
	t.Parallel()
	text := pdfText([]PDFSpan{
		textSpan("50", "Ad Soyad", 260, 315, 32, 0, 150, montserratBold),
		textSpan("51", "Tarih", 90, 500, 12, 0, 40, helvetica),
	}, nil)
	elements, _ := convert(t, text, []string{"50", "51"}, map[string]string{"50": "recipientName", "51": "issueDate"})
	if elements[0].Kind != "recipientName" || elements[0].Text != "" || elements[0].FontFamily != "Montserrat" || elements[0].Fit != "shrink" ||
		elements[1].Kind != "issueDate" || elements[1].Text != "" {
		t.Fatalf("elements %+v", elements)
	}
}

// The placement maps the PDF onto the page: a contained A4 portrait PDF on
// an A4 landscape page is scaled and centred, its text with it.
func TestTextElementsFollowThePlacement(t *testing.T) {
	t.Parallel()
	text := PDFText{Info: PDFInfo{Pages: 1, WidthPt: 595, HeightPt: 842}}
	text.Spans = []PDFSpan{textSpan("1", "Başlık", 100, 100, 20, 0, 100, helvetica)}
	text.Runs = []PDFRun{{SpanIDs: []string{"1"}, Text: "Başlık"}}
	pageW, pageH := 1123.0, 794.0
	box := PlaceBackground(pageW, pageH, 595*pxPerPt, 842*pxPerPt, BackgroundPlacement{Fit: FitContain, X: 0.5, Y: 0.5, Scale: 1})
	elements, _ := textElements(text, []string{"1"}, nil, pageW, pageH, box, nil, sequentialIDs())
	k := box.Height / 842
	el := elements[0]
	if !nearPx(el.X, box.Left+100*k) || !nearPx(el.FontSize, round2(20*k)) || !nearPx(el.Width, 115*k) {
		t.Fatalf("element %+v, box %+v", el, box)
	}
}

// A text the placement puts off the page is left out; a size over 300 px
// is bounded with a warning; a stand-in font is warned of.
func TestTextElementsBoundsAndWarnings(t *testing.T) {
	t.Parallel()
	text := PDFText{Info: PDFInfo{Pages: 1, WidthPt: 842, HeightPt: 595}}
	text.Spans = []PDFSpan{
		textSpan("1", "Dev", 100, 400, 260, 0, 300, PDFFont{Name: "ABCDEF+BrandonGrotesque-Black", Weight: 900}),
		textSpan("2", "Dışarıda", 100, 580, 10, 0, 50, helvetica),
	}
	text.Runs = []PDFRun{{SpanIDs: []string{"1"}, Text: "Dev"}, {SpanIDs: []string{"2"}, Text: "Dışarıda", Line: 1, Block: 1}}
	// Cover, scaled twice, top: the bottom of the PDF is cut off.
	box := PlaceBackground(1123, 794, 842*pxPerPt, 595*pxPerPt, BackgroundPlacement{Fit: FitCover, X: 0.5, Y: 0, Scale: 2})
	elements, warnings := textElements(text, []string{"1", "2"}, nil, 1123, 794, box, nil, sequentialIDs())
	if len(elements) != 1 || elements[0].FontSize != maxTextFontSize || elements[0].SourceFont != "BrandonGrotesque-Black" || elements[0].FontFamily != "Noto Sans" {
		t.Fatalf("elements %+v", elements)
	}
	kinds := map[string]TextWarning{}
	for _, w := range warnings {
		kinds[w.Kind] = w
	}
	if kinds[WarningFontSizeBounded].ElementID != elements[0].ID || kinds[WarningFontSubstituted].From != "BrandonGrotesque-Black" || kinds[WarningFontSubstituted].To != "Noto Sans" {
		t.Fatalf("warnings %+v", warnings)
	}
}
