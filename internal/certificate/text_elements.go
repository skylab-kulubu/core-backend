package certificate

import (
	"math"
	"regexp"
	"sort"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// The kinds a converted text may become instead of a static text
// (TextConversionRequest.Kinds): the same box and style, printing the
// certificate's own value.
var convertibleKinds = map[string]bool{"recipientName": true, "eventName": true, "eventDates": true, "issueDate": true}

// The rules of turning PDF text into elements (SPEC §5.3).
const (
	// textBoxGrowth widens a converted text's box, from the edge its
	// alignment keeps, so a wider stand-in font still fits.
	textBoxGrowth = 0.15
	// textMinFontRatio is a one-line text's minFontSize to its size: a
	// wider stand-in shrinks a little rather than breaking the line.
	textMinFontRatio = 0.85
	// textLineHeight is a one-line text's line height.
	textLineHeight = 1.2
	// alignTolerancePt: lines whose edge is within it share that edge.
	alignTolerancePt = 1.0
	// pageCentreRatio: a one-line text whose middle is this near the
	// page's middle (of the page's extent) is centred.
	pageCentreRatio = 0.02
	// lineSpacingSpread: the baseline gaps of one paragraph differ by at
	// most this part of their mean.
	lineSpacingSpread = 0.1
	maxTextFontSize   = 300
	// A field (kinds) prints values longer than the PDF's placeholder: its
	// box reaches to the page's margin (this part of the page's width) on
	// the side(s) its alignment leaves open, and it may shrink to half.
	fieldMarginRatio   = 0.04
	fieldMinFontRatio  = 0.5
	maxElementTextByte = 4000
)

var hexColorPattern = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// textPiece is a chosen run with its geometry in its own frame: along the
// baseline (u) and across it, down (v), in pt of the visible page.
type textPiece struct {
	run        PDFRun
	first      PDFSpan
	ux, uy     float64 // the text's direction
	start, end float64 // along u
	base, mid  float64 // across: the baseline and the text's middle
	size       float64
	text       string
	kind       string
	styleKey   string
}

// textElements turns the chosen runs (by id) of a PDF's text into layout
// elements (SPEC §5.3), the PDF drawn into box on a pageW × pageH page
// (layout px). kinds turns a run into another kind (convertibleKinds);
// newID names each element. A run alone on its line joins the run on the
// line above when both are of one style, their baselines evenly spaced and
// one of their edges (left, centre, right) shared within a point: a
// paragraph, one element. Every element keeps the PDF font's name
// (SourceFont) and says it came from the background (Origin). It answers a
// warning for each font it stood in for and each size it bounded.
func textElements(text PDFText, chosen []string, kinds map[string]string, pageW, pageH float64, box Rect, uploaded []string, newID func() string) ([]Element, []TextWarning) {
	spans := make(map[string]PDFSpan, len(text.Spans))
	for _, span := range text.Spans {
		spans[span.ID] = span
	}
	want := make(map[string]bool, len(chosen))
	for _, id := range chosen {
		want[id] = true
	}
	sx, sy := box.Width/text.Info.WidthPt, box.Height/text.Info.HeightPt

	var pieces []*textPiece
	for _, run := range text.Runs {
		if !want[run.ID()] {
			continue
		}
		first := spans[run.ID()]
		theta := first.Angle * math.Pi / 180
		p := &textPiece{
			run: run, first: first, ux: math.Cos(theta), uy: math.Sin(theta),
			start: math.Inf(1), end: math.Inf(-1), size: first.Size,
			text: norm.NFC.String(strings.TrimSpace(run.Text)), kind: kinds[run.ID()],
		}
		for _, id := range run.SpanIDs {
			for _, q := range spans[id].Quad {
				along := q[0]*p.ux + q[1]*p.uy
				p.start, p.end = math.Min(p.start, along), math.Max(p.end, along)
			}
		}
		vx, vy := -p.uy, p.ux
		p.base = first.Origin.X*vx + first.Origin.Y*vy
		ascent, descent := first.Font.Ascent, first.Font.Descent
		if ascent <= 0 {
			ascent = 0.9
		}
		if descent >= 0 {
			descent = -0.22
		}
		// The middle of the font's ascent and descent: a stand-in font's
		// line box is centred on it.
		p.mid = p.base - (ascent+descent)/2*p.size
		p.styleKey = strings.Join([]string{first.Font.Name, number(math.Round(first.Size*4) / 4), first.Color, number(first.Opacity), number(math.Round(first.Angle))}, "|")
		pieces = append(pieces, p)
	}

	frame := textFrame{text: text, box: box, sx: sx, sy: sy, pageW: pageW, pageH: pageH}
	var elements []Element
	var warnings []TextWarning
	for _, group := range paragraphs(pieces, text.Runs, spans) {
		el, warns, ok := frame.element(group, uploaded, newID)
		warnings = append(warnings, warns...)
		if ok {
			elements = append(elements, el)
		}
	}
	return elements, warnings
}

// paragraphs groups the pieces into elements: a paragraph's lines
// together, every other piece alone, in reading order.
func paragraphs(pieces []*textPiece, runs []PDFRun, spans map[string]PDFSpan) [][]*textPiece {
	// Every line of each block, by baseline, and whether it was chosen.
	type blockLine struct {
		base  float64
		piece *textPiece // nil: not chosen, or not alone on its line
	}
	chosenByRun := map[string]*textPiece{}
	for _, p := range pieces {
		chosenByRun[p.run.ID()] = p
	}
	byLine := map[int][]PDFRun{}
	for _, run := range runs {
		byLine[run.Line] = append(byLine[run.Line], run)
	}
	blocks := map[int][]blockLine{}
	for _, onLine := range byLine {
		first := spans[onLine[0].ID()]
		theta := first.Angle * math.Pi / 180
		bl := blockLine{base: -math.Sin(theta)*first.Origin.X + math.Cos(theta)*first.Origin.Y}
		if len(onLine) == 1 {
			bl.piece = chosenByRun[onLine[0].ID()]
		}
		blocks[onLine[0].Block] = append(blocks[onLine[0].Block], bl)
	}
	joined := map[*textPiece]bool{}
	var groups [][]*textPiece
	blockIDs := make([]int, 0, len(blocks))
	for id := range blocks {
		blockIDs = append(blockIDs, id)
	}
	sort.Ints(blockIDs)
	for _, id := range blockIDs {
		lines := blocks[id]
		sort.Slice(lines, func(i, j int) bool { return lines[i].base < lines[j].base })
		var seq []*textPiece
		flush := func() {
			for _, g := range splitParagraph(seq) {
				if len(g) > 1 {
					groups = append(groups, g)
					for _, p := range g {
						joined[p] = true
					}
				}
			}
			seq = nil
		}
		for _, line := range lines {
			if line.piece == nil || line.piece.kind != "" || (len(seq) > 0 && seq[0].styleKey != line.piece.styleKey) {
				flush()
				if line.piece != nil && line.piece.kind == "" {
					seq = append(seq, line.piece)
				}
				continue
			}
			seq = append(seq, line.piece)
		}
		flush()
	}
	for _, p := range pieces {
		if !joined[p] {
			groups = append(groups, []*textPiece{p})
		}
	}
	// Reading order: by the first line's baseline, then along it.
	sort.SliceStable(groups, func(i, j int) bool {
		a, b := groups[i][0], groups[j][0]
		if math.Abs(a.base-b.base) > 0.5 {
			return a.base < b.base
		}
		return a.start < b.start
	})
	return groups
}

// splitParagraph takes consecutive lines of one style and answers the runs
// of them that form paragraphs: evenly spaced, sharing an edge.
func splitParagraph(seq []*textPiece) [][]*textPiece {
	if len(seq) < 2 {
		return [][]*textPiece{seq}
	}
	if _, ok := paragraphAlign(seq); ok && evenlySpaced(seq) {
		return [][]*textPiece{seq}
	}
	out := make([][]*textPiece, 0, len(seq))
	for _, p := range seq {
		out = append(out, []*textPiece{p})
	}
	return out
}

func evenlySpaced(seq []*textPiece) bool {
	gaps := make([]float64, 0, len(seq)-1)
	sum := 0.0
	for i := 1; i < len(seq); i++ {
		gap := seq[i].base - seq[i-1].base
		gaps = append(gaps, gap)
		sum += gap
	}
	mean := sum / float64(len(gaps))
	size := seq[0].size
	if mean < 0.8*size || mean > 3*size {
		return false
	}
	for _, gap := range gaps {
		if math.Abs(gap-mean) > lineSpacingSpread*mean {
			return false
		}
	}
	return true
}

// paragraphAlign is the edge a paragraph's lines share: "left", "right" or
// "center", "" for lines of one width (all three), false for none.
func paragraphAlign(seq []*textPiece) (string, bool) {
	spread := func(at func(*textPiece) float64) float64 {
		lo, hi := math.Inf(1), math.Inf(-1)
		for _, p := range seq {
			lo, hi = math.Min(lo, at(p)), math.Max(hi, at(p))
		}
		return hi - lo
	}
	left := spread(func(p *textPiece) float64 { return p.start }) <= alignTolerancePt
	right := spread(func(p *textPiece) float64 { return p.end }) <= alignTolerancePt
	centre := spread(func(p *textPiece) float64 { return (p.start + p.end) / 2 }) <= alignTolerancePt
	switch {
	case left && right:
		return "", true
	case left:
		return "left", true
	case right:
		return "right", true
	case centre:
		return "center", true
	default:
		return "", false
	}
}

// textFrame turns pt of the visible PDF page into layout px.
type textFrame struct {
	text         PDFText
	box          Rect
	sx, sy       float64
	pageW, pageH float64
}

// element is a group's element; false (with a warning) when it falls
// outside the page.
func (f textFrame) element(group []*textPiece, uploaded []string, newID func() string) (Element, []TextWarning, bool) {
	first := group[0]
	font := mapFont(first.first.Font, uploaded)
	vx, vy := -first.uy, first.ux
	start, end := math.Inf(1), math.Inf(-1)
	for _, p := range group {
		start, end = math.Min(start, p.start), math.Max(end, p.end)
	}
	width := end - start
	var align string
	lineHeight := textLineHeight
	height := textLineHeight * first.size
	top := first.mid - height/2
	if len(group) > 1 {
		align, _ = paragraphAlign(group)
		gap := (group[len(group)-1].base - first.base) / float64(len(group)-1)
		lineHeight = gap / first.size
		height = gap * float64(len(group))
		top = first.mid - gap/2
		if font.Substituted {
			// A wider stand-in may wrap a line more: room for one.
			height += gap
		}
	}
	if align == "" {
		align = f.pageAlign(first, start, end)
	}
	grow := textBoxGrowth * width
	switch align {
	case "right":
		start -= grow
	case "center":
		start, end = start-grow/2, end+grow/2
	default:
		end += grow
	}
	// The box's centre in pt, then in layout px; the element turns about
	// its centre (transform-origin:center).
	ca, cv := (start+end)/2, top+height/2
	cx, cy := first.ux*ca+vx*cv, first.uy*ca+vy*cv
	px, py := f.box.Left+cx*f.sx, f.box.Top+cy*f.sy
	alongScale := math.Hypot(first.ux*f.sx, first.uy*f.sy)
	acrossScale := math.Hypot(vx*f.sx, vy*f.sy)
	w, h := (end-start)*alongScale, height*acrossScale
	x, y := px-w/2, py-h/2
	rotation := math.Atan2(first.uy*f.sy, first.ux*f.sx) * 180 / math.Pi
	if math.Abs(rotation) < 0.05 {
		rotation = 0
	}
	if first.kind != "" && rotation == 0 {
		x, w = f.fieldBox(align, x, w)
	}
	// Cut to the page (a text reaching past it, a box grown past it).
	if x < 0 {
		w, x = w+x, 0
	}
	if y < 0 {
		h, y = h+y, 0
	}
	w, h = math.Min(w, f.pageW-x), math.Min(h, f.pageH-y)
	if w < 1 || h < 1 {
		return Element{}, []TextWarning{{SpanID: first.run.ID(), Kind: WarningOutsidePage}}, false
	}
	size := first.size * acrossScale
	id := newID()
	var warnings []TextWarning
	if size > maxTextFontSize {
		warnings = append(warnings, TextWarning{SpanID: first.run.ID(), ElementID: id, Kind: WarningFontSizeBounded, From: number(round2(size)), To: number(maxTextFontSize)})
		size = maxTextFontSize
	}
	if font.Substituted {
		warnings = append(warnings, TextWarning{SpanID: first.run.ID(), ElementID: id, Kind: WarningFontSubstituted, From: sourceFontLabel(first.first.Font), To: font.Family})
	}
	color := first.first.Color
	if !hexColorPattern.MatchString(color) {
		color = "#000000"
	}
	el := Element{
		ID: id, Kind: "staticText",
		X: round2(x), Y: round2(y), Width: round2(w), Height: round2(h),
		FontFamily: font.Family, FontSize: round2(math.Max(size, 1)), FontWeight: font.Weight, FontStyle: font.Style,
		LineHeight: round2(math.Min(math.Max(lineHeight, 0.5), 5)), Color: strings.ToLower(color), Align: align,
		Rotation: round2(rotation), Origin: OriginBackgroundText, SourceFont: truncateRunes(stripSubset(first.first.Font.Name), maxSourceFontRunes),
	}
	if opacity := first.first.Opacity; opacity < 1 && opacity >= 0 {
		o := math.Round(opacity*1000) / 1000
		el.Opacity = &o
	}
	if len(group) == 1 {
		// One line shrinks to fit (it never breaks); a paragraph keeps its
		// lines and wraps.
		el.Fit = "shrink"
		el.MinFontSize = round2(textMinFontRatio * el.FontSize)
	}
	if first.kind != "" {
		el.Kind = first.kind
		el.MinFontSize = round2(fieldMinFontRatio * el.FontSize)
	} else {
		lines := make([]string, len(group))
		for i, p := range group {
			lines[i] = p.text
		}
		full := strings.Join(lines, "\n")
		el.Text = truncateBytes(full, maxElementTextByte)
		if len(el.Text) < len(full) {
			warnings = append(warnings, TextWarning{SpanID: first.run.ID(), ElementID: id, Kind: WarningTextCut,
				From: number(float64(len(full))), To: number(float64(len(el.Text)))})
		}
	}
	return el, warnings, true
}

// fieldBox widens an upright field's box (x, w in px) to the page's
// margin on the side(s) its alignment leaves open, never narrower than it
// was: a recipient's name is longer than the PDF's "Ad Soyad".
func (f textFrame) fieldBox(align string, x, w float64) (float64, float64) {
	margin := fieldMarginRatio * f.pageW
	switch align {
	case "right":
		left := math.Min(x, margin)
		return left, x + w - left
	case "center":
		c := x + w/2
		half := math.Max(w/2, math.Min(c-margin, f.pageW-margin-c))
		return c - half, 2 * half
	default:
		return x, math.Max(w, f.pageW-margin-x)
	}
}

// pageAlign is a one-line text's alignment: centred when its middle is
// within 2% of the page's middle along it, left otherwise.
func (f textFrame) pageAlign(p *textPiece, start, end float64) string {
	// The layout page's middle and extent in the PDF's pt.
	cx := (f.pageW/2 - f.box.Left) / f.sx
	cy := (f.pageH/2 - f.box.Top) / f.sy
	extent := math.Abs(p.ux)*f.pageW/f.sx + math.Abs(p.uy)*f.pageH/f.sy
	if math.Abs((start+end)/2-(cx*p.ux+cy*p.uy)) <= pageCentreRatio*extent {
		return "center"
	}
	return "left"
}

// sourceFontLabel is how a warning names the PDF's font.
func sourceFontLabel(font PDFFont) string {
	if name := stripSubset(strings.TrimSpace(font.Name)); name != "" {
		return name
	}
	return font.Family
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

func truncateRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}

func truncateBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }
