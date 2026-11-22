package mapimage

import (
	"context"
	"errors"
	"html"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ErrNoMap is a point the archive has no map of: outside its extract, or
// where it holds nothing.
var ErrNoMap = errors.New("mapimage: no map of this point in the archive")

// Options is the image's frame: its size in CSS pixels, the zoom it is
// drawn at (Web Mercator, 256-pixel tiles) and the pixel ratio of the
// picture taken of it.
type Options struct {
	Width, Height int
	Zoom          int
	Scale         int
}

// DefaultOptions is the map of the ticket and the ticket page: 640 × 240
// at zoom 16 (about 1.1 km across in Istanbul: the building and the streets
// around it), taken at twice the size, 1280 × 480 pixels. On the PDF it is
// 154 mm wide, about 210 dpi.
var DefaultOptions = Options{Width: 640, Height: 240, Zoom: 16, Scale: 2}

// The map's look: the ticket's greys and the club's purple pin (Kurumsal).
const (
	colorWater     = "#D6DBE0"
	colorEarth     = "#F2F2F0"
	colorGreen     = "#E4E7E2"
	colorCampus    = "#EAE8E3"
	colorPlaza     = "#F8F8F7"
	colorBuilding  = "#DDDDDB"
	colorBuildEdge = "#CECECC"
	colorPath      = "#C2C2C2"
	colorRail      = "#BDBDBD"
	colorLabel     = "#5C5C5C"
	colorPin       = "#7C3AED"
)

// layersDrawn are the basemap's layers the map reads (Protomaps basemap
// schema v4).
var layersDrawn = map[string]bool{"earth": true, "water": true, "landcover": true, "landuse": true, "buildings": true, "roads": true, "transit": true, "pois": true}

var (
	greenKinds  = map[string]bool{"park": true, "garden": true, "grass": true, "forest": true, "wood": true, "meadow": true, "pitch": true, "playground": true, "golf_course": true, "cemetery": true, "recreation_ground": true, "nature_reserve": true, "grassland": true, "scrub": true, "farmland": true, "allotments": true, "village_green": true, "dog_park": true, "zoo": true}
	campusKinds = map[string]bool{"university": true, "college": true, "school": true, "hospital": true, "kindergarten": true}
	plazaKinds  = map[string]bool{"pedestrian": true, "platform": true}
	poiKinds    = map[string]bool{"university": true, "college": true, "hospital": true, "stadium": true, "library": true, "museum": true, "park": true, "station": true, "train_station": true}
)

// roadStyle is how one class of road is drawn: its casing and its fill
// (widths in CSS pixels at zoom 16), or a dashed line.
type roadStyle struct {
	rank           int
	casing, fill   float64
	casingColor    string
	fillColor      string
	dashed         bool
	labelPriority  int
	labelSizeRatio float64
}

var roadStyles = map[string]roadStyle{
	"highway":    {rank: 4, casing: 9, fill: 7.2, casingColor: "#ABABAB", fillColor: "#F8F8F8", labelPriority: 4},
	"major_road": {rank: 3, casing: 7, fill: 5.4, casingColor: "#BFBFBF", fillColor: "#FFFFFF", labelPriority: 3},
	"minor_road": {rank: 2, casing: 5, fill: 3.8, casingColor: "#D0D0D0", fillColor: "#FFFFFF", labelPriority: 2},
	"other":      {rank: 1, casing: 3.4, fill: 2.4, casingColor: "#D6D6D6", fillColor: "#FFFFFF", labelPriority: 1},
	"path":       {rank: 0, fill: 0.9, fillColor: colorPath, dashed: true},
}

// frame places tile coordinates in the image.
type frame struct {
	o         Options
	left, top float64
	tileSize  float64
}

func (f frame) at(tx, ty uint32, extent float64, p point) point {
	return point{
		x: float64(tx)*f.tileSize + p.x/extent*f.tileSize - f.left,
		y: float64(ty)*f.tileSize + p.y/extent*f.tileSize - f.top,
	}
}

// project is a point's place on the world at zoom z, in pixels.
func project(lat, lng float64, z int) (float64, float64) {
	world := 256 * math.Exp2(float64(z))
	lat = math.Max(-85.05112878, math.Min(85.05112878, lat))
	phi := lat * math.Pi / 180
	return (lng + 180) / 360 * world, (1 - math.Log(math.Tan(phi)+1/math.Cos(phi))/math.Pi) / 2 * world
}

// pathData collects the SVG path data of one drawing.
type pathData struct{ b strings.Builder }

func num(b *strings.Builder, v float64) {
	b.WriteString(strconv.FormatFloat(math.Round(v*10)/10, 'f', -1, 64))
}

func (p *pathData) add(points []point, closed bool) {
	if len(points) < 2 {
		return
	}
	for i, pt := range points {
		if i == 0 {
			p.b.WriteByte('M')
		} else {
			p.b.WriteByte('L')
		}
		num(&p.b, pt.x)
		p.b.WriteByte(' ')
		num(&p.b, pt.y)
	}
	if closed {
		p.b.WriteByte('Z')
	}
}

func (p *pathData) empty() bool { return p.b.Len() == 0 }

// bounds is a box in the image.
type bounds struct{ x0, y0, x1, y1 float64 }

func boxOf(points []point) bounds {
	b := bounds{math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)}
	for _, p := range points {
		b.x0, b.y0, b.x1, b.y1 = math.Min(b.x0, p.x), math.Min(b.y0, p.y), math.Max(b.x1, p.x), math.Max(b.y1, p.y)
	}
	return b
}

func (b bounds) overlaps(c bounds) bool {
	return b.x0 < c.x1 && c.x0 < b.x1 && b.y0 < c.y1 && c.y0 < b.y1
}

func (b bounds) grow(by float64) bounds {
	return bounds{b.x0 - by, b.y0 - by, b.x1 + by, b.y1 + by}
}

// candidate is a name that may be drawn: along a road, or at a place.
type candidate struct {
	name     string
	points   []point
	priority int
	length   float64
	along    bool
}

// RenderSVG draws the map around the point from the archive's tiles: land,
// water, green and campus areas, buildings, roads, railways, the names of
// the larger places and of the streets that have room for them, and the
// pin on the point, in the middle.
func RenderSVG(ctx context.Context, a *Archive, lat, lng float64, o Options) (string, error) {
	svg, _, err := render(ctx, a, lat, lng, o)
	return svg, err
}

// Stats counts what a map drew: the features of each layer that reached
// the frame, and the names written.
type Stats struct {
	Features map[string]int `json:"features"`
	Labels   int            `json:"labels"`
}

func render(ctx context.Context, a *Archive, lat, lng float64, o Options) (string, Stats, error) {
	stats := Stats{Features: map[string]int{}}
	if !a.Covers(lat, lng) {
		return "", stats, ErrNoMap
	}
	source := min(o.Zoom, int(a.header.MaxZoom))
	cx, cy := project(lat, lng, o.Zoom)
	f := frame{o: o, left: cx - float64(o.Width)/2, top: cy - float64(o.Height)/2, tileSize: 256 * math.Exp2(float64(o.Zoom-source))}
	view := bounds{0, 0, float64(o.Width), float64(o.Height)}
	last := uint32(math.Exp2(float64(source))) - 1
	clamp := func(v float64) uint32 { return uint32(math.Max(0, math.Min(float64(last), math.Floor(v)))) }
	tx0, tx1 := clamp(f.left/f.tileSize), clamp((f.left+float64(o.Width))/f.tileSize)
	ty0, ty1 := clamp(f.top/f.tileSize), clamp((f.top+float64(o.Height))/f.tileSize)

	var water, earth, green, campus, plaza, buildings, rail pathData
	waterLines := map[float64]*pathData{}
	casings := map[string]*pathData{}
	fills := map[string]*pathData{}
	var candidates []candidate
	drew := false
	for tx := tx0; tx <= tx1; tx++ {
		for ty := ty0; ty <= ty1; ty++ {
			raw, err := a.Tile(ctx, uint8(source), tx, ty)
			if err != nil {
				return "", stats, err
			}
			if raw == nil {
				continue
			}
			layers, err := decodeTile(raw, layersDrawn)
			if err != nil {
				return "", stats, err
			}
			for _, l := range layers {
				for _, feat := range l.features {
					parts := make([][]point, 0, len(feat.parts))
					for _, part := range feat.parts {
						placed := make([]point, len(part))
						for i, p := range part {
							placed[i] = f.at(tx, ty, l.extent, p)
						}
						if boxOf(placed).overlaps(view.grow(12)) {
							parts = append(parts, placed)
						}
					}
					if len(parts) == 0 {
						continue
					}
					drew = true
					stats.Features[l.name]++
					kind := feat.props["kind"]
					polygon := feat.kind == geomPolygon
					switch l.name {
					case "earth":
						if polygon {
							addAll(&earth, parts, true)
						}
					case "water":
						switch feat.kind {
						case geomPolygon:
							addAll(&water, parts, true)
						case geomLine:
							width := 1.5
							if kind == "river" || kind == "canal" {
								width = 3
							}
							if waterLines[width] == nil {
								waterLines[width] = &pathData{}
							}
							addAll(waterLines[width], parts, false)
						}
					case "landcover", "landuse":
						if !polygon {
							continue
						}
						switch {
						case greenKinds[kind]:
							addAll(&green, parts, true)
						case campusKinds[kind]:
							addAll(&campus, parts, true)
						case plazaKinds[kind]:
							addAll(&plaza, parts, true)
						}
					case "buildings":
						if polygon {
							addAll(&buildings, parts, true)
						}
					case "transit":
						if feat.kind == geomLine && (kind == "rail" || feat.props["kind_detail"] == "rail" || feat.props["kind_detail"] == "subway" || feat.props["kind_detail"] == "light_rail") {
							addAll(&rail, parts, false)
						}
					case "roads":
						if feat.kind != geomLine {
							continue
						}
						style, ok := roadStyles[kind]
						if !ok {
							continue
						}
						key := kind
						if feat.props["is_tunnel"] == "true" {
							continue
						}
						if fills[key] == nil {
							fills[key], casings[key] = &pathData{}, &pathData{}
						}
						addAll(fills[key], parts, false)
						if style.casing > 0 {
							addAll(casings[key], parts, false)
						}
						if name := nameOf(feat.props); name != "" && style.labelPriority > 0 {
							for _, part := range parts {
								candidates = append(candidates, candidate{name: name, points: part, priority: style.labelPriority, length: lengthIn(part, view), along: true})
							}
						}
					case "pois":
						if feat.kind == geomPoint && poiKinds[kind] && atZoom(feat.props, o.Zoom) {
							if name := nameOf(feat.props); name != "" {
								candidates = append(candidates, candidate{name: name, points: parts[0][:1], priority: 10})
							}
						}
					}
				}
			}
		}
	}
	if !drew {
		return "", stats, ErrNoMap
	}

	var b strings.Builder
	w, h := strconv.Itoa(o.Width), strconv.Itoa(o.Height)
	b.WriteString(`<svg xmlns="http://www.w3.org/2000/svg" width="` + strconv.Itoa(o.Width*o.Scale) + `" height="` + strconv.Itoa(o.Height*o.Scale) +
		`" viewBox="0 0 ` + w + ` ` + h + `" role="img" aria-label="Etkinlik yeri haritası">`)
	b.WriteString(`<rect width="` + w + `" height="` + h + `" fill="` + colorWater + `"/>`)
	fill := func(p *pathData, color string, extra string) {
		if !p.empty() {
			b.WriteString(`<path fill="` + color + `" fill-rule="nonzero"` + extra + ` d="` + p.b.String() + `"/>`)
		}
	}
	stroke := func(p *pathData, color string, width float64, extra string) {
		if !p.empty() {
			b.WriteString(`<path fill="none" stroke="` + color + `" stroke-width="` + strconv.FormatFloat(width, 'f', -1, 64) +
				`" stroke-linecap="round" stroke-linejoin="round"` + extra + ` d="` + p.b.String() + `"/>`)
		}
	}
	fill(&earth, colorEarth, "")
	fill(&green, colorGreen, "")
	fill(&campus, colorCampus, "")
	fill(&plaza, colorPlaza, "")
	fill(&water, colorWater, "")
	widths := make([]float64, 0, len(waterLines))
	for width := range waterLines {
		widths = append(widths, width)
	}
	sort.Float64s(widths)
	for _, width := range widths {
		stroke(waterLines[width], colorWater, width, "")
	}
	fill(&buildings, colorBuilding, ` stroke="`+colorBuildEdge+`" stroke-width=".5"`)
	stroke(&rail, colorRail, 1.2, ` stroke-dasharray="4 2"`)
	kinds := make([]string, 0, len(fills))
	for kind := range fills {
		kinds = append(kinds, kind)
	}
	sort.Slice(kinds, func(i, j int) bool { return roadStyles[kinds[i]].rank < roadStyles[kinds[j]].rank })
	for _, kind := range kinds {
		if s := roadStyles[kind]; s.casing > 0 {
			stroke(casings[kind], s.casingColor, s.casing, "")
		}
	}
	for _, kind := range kinds {
		s := roadStyles[kind]
		extra := ""
		if s.dashed {
			extra = ` stroke-dasharray="2 1.5"`
		}
		stroke(fills[kind], s.fillColor, s.fill, extra)
	}
	stats.Labels = writeLabels(&b, candidates, view, float64(o.Width)/2, float64(o.Height)/2)
	writePin(&b, float64(o.Width)/2, float64(o.Height)/2)
	b.WriteString(`</svg>`)
	return b.String(), stats, nil
}

func addAll(p *pathData, parts [][]point, closed bool) {
	for _, part := range parts {
		p.add(part, closed)
	}
}

// nameOf is a feature's name in Turkish where the data has it.
func nameOf(props map[string]string) string {
	if n := strings.TrimSpace(props["name:tr"]); n != "" {
		return n
	}
	return strings.TrimSpace(props["name"])
}

// atZoom reports whether a place's name is shown at zoom z.
func atZoom(props map[string]string, z int) bool {
	mz, err := strconv.Atoi(props["min_zoom"])
	return err != nil || mz <= z
}

// lengthIn is how long the line runs inside the frame.
func lengthIn(points []point, view bounds) float64 {
	var total float64
	for i := 1; i < len(points); i++ {
		if a, b, ok := clipSegment(points[i-1], points[i], view); ok {
			total += math.Hypot(b.x-a.x, b.y-a.y)
		}
	}
	return total
}

func inside(p point, b bounds) bool {
	return p.x >= b.x0 && p.x <= b.x1 && p.y >= b.y0 && p.y <= b.y1
}

// The names' type: Manrope, the club's (the page embeds it).
const (
	labelSize = 10
	placeSize = 10.5
	// labelAdvance is Manrope SemiBold's mean advance, in ems: an
	// estimate of a name's length.
	labelAdvance = 0.58
	maxLabels    = 10
)

// writeLabels draws the names that fit without touching another or the
// pin: the larger places first, then the streets by class and length. A
// street's name runs along its longest stretch in the frame, left to right,
// when the stretch is long and straight enough.
func writeLabels(b *strings.Builder, candidates []candidate, view bounds, pinX, pinY float64) int {
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].priority != candidates[j].priority {
			return candidates[i].priority > candidates[j].priority
		}
		return candidates[i].length > candidates[j].length
	})
	taken := []bounds{{pinX - 12, pinY - 30, pinX + 12, pinY + 2}}
	named := map[string]bool{}
	drawn := 0
	var defs, texts strings.Builder
	for _, c := range candidates {
		if drawn >= maxLabels || named[c.name] {
			continue
		}
		if !c.along {
			p := c.points[0]
			width := labelAdvance * placeSize * float64(utf8.RuneCountInString(c.name))
			box := bounds{p.x - width/2, p.y - placeSize, p.x + width/2, p.y + 2}
			if !contains(view.grow(-4), box) || collides(box.grow(3), taken) {
				continue
			}
			taken = append(taken, box)
			named[c.name] = true
			drawn++
			texts.WriteString(`<text x="`)
			num(&texts, p.x)
			texts.WriteString(`" y="`)
			num(&texts, p.y)
			texts.WriteString(`" text-anchor="middle" font-size="` + strconv.FormatFloat(placeSize, 'f', -1, 64) + `" font-weight="700" fill="` + colorLabel + `">` + html.EscapeString(c.name) + `</text>`)
			continue
		}
		run := longestRun(c.points, view.grow(-6))
		width := labelAdvance * labelSize * float64(utf8.RuneCountInString(c.name))
		if len(run) < 2 || runLength(run) < width*1.15 || turning(run) > math.Pi/3 {
			continue
		}
		box := boxOf(run).grow(labelSize / 2)
		if collides(box, taken) {
			continue
		}
		if run[len(run)-1].x < run[0].x {
			for i, j := 0, len(run)-1; i < j; i, j = i+1, j-1 {
				run[i], run[j] = run[j], run[i]
			}
		}
		taken = append(taken, box)
		named[c.name] = true
		id := "y" + strconv.Itoa(drawn)
		drawn++
		var d pathData
		d.add(run, false)
		defs.WriteString(`<path id="` + id + `" d="` + d.b.String() + `"/>`)
		texts.WriteString(`<text font-size="` + strconv.FormatFloat(labelSize, 'f', -1, 64) + `" font-weight="600" fill="` + colorLabel + `" dy="3"><textPath href="#` + id +
			`" startOffset="50%" text-anchor="middle">` + html.EscapeString(c.name) + `</textPath></text>`)
	}
	if drawn == 0 {
		return 0
	}
	b.WriteString(`<defs>` + defs.String() + `</defs><g font-family="Manrope,'Noto Sans','Liberation Sans',Arial,sans-serif" stroke="#FFFFFF" stroke-width="2.8" stroke-linejoin="round" paint-order="stroke">`)
	b.WriteString(texts.String())
	b.WriteString(`</g>`)
	return drawn
}

func contains(outer, inner bounds) bool {
	return inner.x0 >= outer.x0 && inner.y0 >= outer.y0 && inner.x1 <= outer.x1 && inner.y1 <= outer.y1
}

func collides(box bounds, taken []bounds) bool {
	for _, t := range taken {
		if box.overlaps(t) {
			return true
		}
	}
	return false
}

// longestRun is the longest stretch of the line inside the box, its
// segments cut at the box's edges.
func longestRun(points []point, box bounds) []point {
	var best, cur []point
	flush := func() {
		if runLength(cur) > runLength(best) {
			best = cur
		}
		cur = nil
	}
	for i := 1; i < len(points); i++ {
		a, b, ok := clipSegment(points[i-1], points[i], box)
		if !ok {
			flush()
			continue
		}
		if len(cur) > 0 && cur[len(cur)-1] != a {
			flush()
		}
		if len(cur) == 0 {
			cur = append(cur, a)
		}
		cur = append(cur, b)
		if b != points[i] {
			flush()
		}
	}
	flush()
	return append([]point(nil), best...)
}

// clipSegment is the part of the segment inside the box (Liang–Barsky).
func clipSegment(a, b point, box bounds) (point, point, bool) {
	t0, t1 := 0.0, 1.0
	dx, dy := b.x-a.x, b.y-a.y
	for _, edge := range [4][2]float64{{-dx, a.x - box.x0}, {dx, box.x1 - a.x}, {-dy, a.y - box.y0}, {dy, box.y1 - a.y}} {
		p, q := edge[0], edge[1]
		if p == 0 {
			if q < 0 {
				return point{}, point{}, false
			}
			continue
		}
		r := q / p
		if p < 0 {
			t0 = math.Max(t0, r)
		} else {
			t1 = math.Min(t1, r)
		}
		if t0 > t1 {
			return point{}, point{}, false
		}
	}
	start, end := a, b
	if t0 > 0 {
		start = point{a.x + t0*dx, a.y + t0*dy}
	}
	if t1 < 1 {
		end = point{a.x + t1*dx, a.y + t1*dy}
	}
	return start, end, start != end
}

func runLength(points []point) float64 {
	var total float64
	for i := 1; i < len(points); i++ {
		total += math.Hypot(points[i].x-points[i-1].x, points[i].y-points[i-1].y)
	}
	return total
}

// turning is how far the line turns along its way, in radians.
func turning(points []point) float64 {
	var total float64
	for i := 2; i < len(points); i++ {
		a := math.Atan2(points[i-1].y-points[i-2].y, points[i-1].x-points[i-2].x)
		c := math.Atan2(points[i].y-points[i-1].y, points[i].x-points[i-1].x)
		d := math.Abs(c - a)
		if d > math.Pi {
			d = 2*math.Pi - d
		}
		total += d
	}
	return total
}

// writePin draws the club's pin with its tip on the point.
func writePin(b *strings.Builder, x, y float64) {
	b.WriteString(`<g transform="translate(`)
	num(b, x)
	b.WriteByte(' ')
	num(b, y)
	b.WriteString(`)"><ellipse cx="0" cy="0" rx="5" ry="1.8" fill="#000000" fill-opacity=".18"/>` +
		`<path d="M0 0C-2.5-6.5-9-10.5-9-17.5a9 9 0 1 1 18 0C9-10.5 2.5-6.5 0 0Z" fill="` + colorPin + `" stroke="#FFFFFF" stroke-width="1.4"/>` +
		`<circle cx="0" cy="-17.5" r="3.3" fill="#FFFFFF"/></g>`)
}
