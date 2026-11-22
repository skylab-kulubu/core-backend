package mapimage

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The PMTiles spec's own ids: zoom 0 is 0; zoom 1 walks (0,0), (0,1),
// (1,1), (1,0); zoom 2 starts at 5.
func TestTileID(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		z    uint8
		x, y uint32
		id   uint64
	}{
		{0, 0, 0, 0}, {1, 0, 0, 1}, {1, 0, 1, 2}, {1, 1, 1, 3}, {1, 1, 0, 4}, {2, 0, 0, 5}, {2, 3, 0, 20},
	} {
		if got := TileID(tc.z, tc.x, tc.y); got != tc.id {
			t.Errorf("TileID(%d,%d,%d) = %d, want %d", tc.z, tc.x, tc.y, got, tc.id)
		}
	}
	// Every id of a zoom once.
	seen := map[uint64]bool{}
	for x := range uint32(16) {
		for y := range uint32(16) {
			id := TileID(4, x, y)
			if id < 85 || id >= 85+256 || seen[id] {
				t.Fatalf("TileID(4,%d,%d) = %d", x, y, id)
			}
			seen[id] = true
		}
	}
}

// --- a vector tile and an archive, written as the spec says, for the tests.

type pbuf struct{ b []byte }

func (p *pbuf) varint(v uint64) { p.b = binary.AppendUvarint(p.b, v) }
func (p *pbuf) key(field, typ uint64) {
	p.varint(field<<3 | typ)
}
func (p *pbuf) bytes(field uint64, b []byte) {
	p.key(field, 2)
	p.varint(uint64(len(b)))
	p.b = append(p.b, b...)
}
func (p *pbuf) uint(field, v uint64) {
	p.key(field, 0)
	p.varint(v)
}

type testFeature struct {
	kind  int
	props map[string]string
	parts [][][2]int
}

func zz(v int) uint32 { return uint32((v << 1) ^ (v >> 31)) }

func encodeGeometry(kind int, parts [][][2]int) []byte {
	var cmds []uint32
	x, y := 0, 0
	for _, part := range parts {
		cmds = append(cmds, 1|1<<3, zz(part[0][0]-x), zz(part[0][1]-y))
		x, y = part[0][0], part[0][1]
		rest := part[1:]
		if kind == geomPolygon {
			rest = rest[:len(rest)-1]
		}
		if len(rest) > 0 {
			cmds = append(cmds, 2|uint32(len(rest))<<3)
			for _, p := range rest {
				cmds = append(cmds, zz(p[0]-x), zz(p[1]-y))
				x, y = p[0], p[1]
			}
		}
		if kind == geomPolygon {
			cmds = append(cmds, 7|1<<3)
		}
	}
	var p pbuf
	for _, c := range cmds {
		p.varint(uint64(c))
	}
	return p.b
}

func encodeTile(layers map[string][]testFeature) []byte {
	names := make([]string, 0, len(layers))
	for name := range layers {
		names = append(names, name)
	}
	sort.Strings(names)
	var tile pbuf
	for _, name := range names {
		var l pbuf
		l.uint(15, 2)
		l.bytes(1, []byte(name))
		keys, values := []string{}, []string{}
		index := func(list *[]string, s string) uint64 {
			for i, v := range *list {
				if v == s {
					return uint64(i)
				}
			}
			*list = append(*list, s)
			return uint64(len(*list) - 1)
		}
		var features [][]byte
		for _, f := range layers[name] {
			var fb pbuf
			var tags pbuf
			propKeys := make([]string, 0, len(f.props))
			for k := range f.props {
				propKeys = append(propKeys, k)
			}
			sort.Strings(propKeys)
			for _, k := range propKeys {
				tags.varint(index(&keys, k))
				tags.varint(index(&values, f.props[k]))
			}
			fb.bytes(2, tags.b)
			fb.uint(3, uint64(f.kind))
			fb.bytes(4, encodeGeometry(f.kind, f.parts))
			features = append(features, fb.b)
		}
		for _, f := range features {
			l.bytes(2, f)
		}
		for _, k := range keys {
			l.bytes(3, []byte(k))
		}
		for _, v := range values {
			var vb pbuf
			vb.bytes(1, []byte(v))
			l.bytes(4, vb.b)
		}
		l.uint(5, 4096)
		tile.bytes(3, l.b)
	}
	return tile.b
}

func gz(t *testing.T, b []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	w := gzip.NewWriter(&out)
	if _, err := w.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

type testTile struct {
	z    uint8
	x, y uint32
	data []byte
}

// writeArchive is a clustered PMTiles v3 archive of the tiles (gzip'd, one
// root directory), with the bounds given.
func writeArchive(t *testing.T, tiles []testTile, minLon, minLat, maxLon, maxLat float64) []byte {
	t.Helper()
	sort.Slice(tiles, func(i, j int) bool { return TileID(tiles[i].z, tiles[i].x, tiles[i].y) < TileID(tiles[j].z, tiles[j].x, tiles[j].y) })
	var data []byte
	var dir pbuf
	dir.varint(uint64(len(tiles)))
	var last uint64
	offsets, lengths := []uint64{}, []uint64{}
	for _, tile := range tiles {
		id := TileID(tile.z, tile.x, tile.y)
		dir.varint(id - last)
		last = id
		compressed := gz(t, tile.data)
		offsets, lengths = append(offsets, uint64(len(data))), append(lengths, uint64(len(compressed)))
		data = append(data, compressed...)
	}
	for range tiles {
		dir.varint(1)
	}
	for _, l := range lengths {
		dir.varint(l)
	}
	for _, o := range offsets {
		dir.varint(o + 1)
	}
	root := gz(t, dir.b)
	header := make([]byte, headerLength)
	copy(header, "PMTiles")
	header[7] = 3
	put := func(at int, v uint64) { binary.LittleEndian.PutUint64(header[at:], v) }
	put(8, headerLength)
	put(16, uint64(len(root)))
	put(40, uint64(headerLength+len(root)))
	put(48, 0)
	put(56, uint64(headerLength+len(root)))
	put(64, uint64(len(data)))
	header[96], header[97], header[98], header[99], header[100], header[101] = 1, compressionGzip, compressionGzip, tileTypeMVT, 0, 15
	deg := func(at int, v float64) { binary.LittleEndian.PutUint32(header[at:], uint32(int32(math.Round(v*1e7)))) }
	deg(102, minLon)
	deg(106, minLat)
	deg(110, maxLon)
	deg(114, maxLat)
	out := append(header, root...)
	return append(out, data...)
}

// bytesSource serves the archive from memory and counts the reads.
type bytesSource struct {
	b     []byte
	mu    sync.Mutex
	reads int
}

func (s *bytesSource) ReadRange(_ context.Context, offset, length uint64) ([]byte, error) {
	s.mu.Lock()
	s.reads++
	s.mu.Unlock()
	if offset >= uint64(len(s.b)) {
		return nil, ErrUnavailable
	}
	end := min(offset+length, uint64(len(s.b)))
	return s.b[offset:end], nil
}

// davutpasa is the test's point and the tiles around it at zoom 15: land,
// a lake, a park, buildings, two named roads, a footpath and a campus.
const davutLat, davutLng = 41.025376, 28.889648

func tileOf(lat, lng float64, z uint8) (uint32, uint32) {
	x, y := project(lat, lng, int(z))
	return uint32(x / 256), uint32(y / 256)
}

func testArchive(t *testing.T) []byte {
	t.Helper()
	x, y := tileOf(davutLat, davutLng, 15)
	// The point in its tile's units; the view is about ±2560 across and
	// ±960 up and down of it (zoom 16 drawn from zoom 15).
	wx, wy := project(davutLat, davutLng, 15)
	px, py := int((wx/256-float64(x))*4096), int((wy/256-float64(y))*4096)
	box := func(x0, y0, x1, y1 int) [][][2]int {
		return [][][2]int{{{x0, y0}, {x1, y0}, {x1, y1}, {x0, y1}, {x0, y0}}}
	}
	whole := box(-64, -64, 4160, 4160)
	var tiles []testTile
	for dx := uint32(0); dx < 3; dx++ {
		for dy := uint32(0); dy < 3; dy++ {
			layers := map[string][]testFeature{"earth": {{kind: geomPolygon, props: map[string]string{"kind": "earth"}, parts: whole}}}
			if dx == 1 && dy == 1 {
				layers["water"] = []testFeature{{kind: geomPolygon, props: map[string]string{"kind": "water"}, parts: box(px+1200, py-800, px+1700, py-500)}}
				layers["landuse"] = []testFeature{
					{kind: geomPolygon, props: map[string]string{"kind": "park"}, parts: box(px-1800, py-800, px-1000, py+800)},
					{kind: geomPolygon, props: map[string]string{"kind": "university"}, parts: box(px-900, py-900, px+900, py+900)},
				}
				layers["buildings"] = []testFeature{{kind: geomPolygon, props: map[string]string{"kind": "building"}, parts: box(px+200, py-200, px+500, py+100)}}
				layers["roads"] = []testFeature{
					{kind: geomLine, props: map[string]string{"kind": "major_road", "name": "Davutpaşa Caddesi"}, parts: [][][2]int{{{px + 3000, py - 400}, {px, py - 400}, {px - 3000, py - 410}}}},
					{kind: geomLine, props: map[string]string{"kind": "minor_road", "name": "Ali <Sokak> & Co", "name:tr": "Çiçek Sokağı"}, parts: [][][2]int{{{px - 1500, py + 300}, {px + 1500, py + 300}}}},
					{kind: geomLine, props: map[string]string{"kind": "path"}, parts: [][][2]int{{{px - 2000, py - 900}, {px + 2000, py + 900}}}},
					{kind: geomLine, props: map[string]string{"kind": "major_road", "is_tunnel": "true", "name": "Tünel Yolu"}, parts: [][][2]int{{{px - 3000, py + 150}, {px + 3000, py + 150}}}},
				}
				layers["pois"] = []testFeature{{kind: geomPoint, props: map[string]string{"kind": "university", "name": "Yıldız Teknik Üniversitesi", "min_zoom": "14"}, parts: [][][2]int{{{px + 1000, py + 600}}}}}
			}
			tiles = append(tiles, testTile{z: 15, x: x - 1 + dx, y: y - 1 + dy, data: encodeTile(layers)})
		}
	}
	return writeArchive(t, tiles, 28.8, 40.9, 29.1, 41.2)
}

func openTest(t *testing.T) (*Archive, *bytesSource) {
	t.Helper()
	src := &bytesSource{b: testArchive(t)}
	a, err := Open(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	return a, src
}

// The archive is read by its header and root directory, then tile by
// tile; a tile it does not have is nil.
func TestArchiveReadsTiles(t *testing.T) {
	t.Parallel()
	a, _ := openTest(t)
	h := a.Header()
	if h.MaxZoom != 15 || h.TileType != tileTypeMVT || math.Abs(h.MinLon-28.8) > 1e-6 || !a.Covers(davutLat, davutLng) || a.Covers(60, 10) {
		t.Fatalf("header %+v", h)
	}
	x, y := tileOf(davutLat, davutLng, 15)
	raw, err := a.Tile(context.Background(), 15, x, y)
	if err != nil || raw == nil {
		t.Fatalf("tile %v", err)
	}
	layers, err := decodeTile(raw, map[string]bool{"roads": true})
	if err != nil || len(layers) != 1 || len(layers[0].features) != 4 || layers[0].features[0].props["name"] != "Davutpaşa Caddesi" {
		t.Fatalf("roads %+v %v", layers, err)
	}
	if line := layers[0].features[0].parts[0]; len(line) != 3 || line[1].y != line[0].y || line[2].y != line[0].y-10 {
		t.Fatalf("geometry %v", line)
	}
	if none, err := a.Tile(context.Background(), 15, x+10, y); none != nil || err != nil {
		t.Fatalf("a tile the archive does not have: %d bytes, %v", len(none), err)
	}
	if none, err := a.Tile(context.Background(), 16, x, y); none != nil || err != nil {
		t.Fatal("a zoom the archive does not have")
	}
	if _, err := Open(context.Background(), &bytesSource{b: []byte("bir arşiv değil, uzun bir metin ki başlık kadar olsun; bir arşiv değil, uzun bir metin ki başlık kadar olsun; bir arşiv değil...")}); !errors.Is(err, ErrNotArchive) {
		t.Fatalf("not an archive: %v", err)
	}
}

// The map: land, water, the park and the campus, the building, the roads
// with their casings and the footpath dashed (the tunnel left out), the
// streets' names along them in Turkish and escaped, the campus's name, and
// the pin on the point, in the middle.
func TestRenderSVG(t *testing.T) {
	t.Parallel()
	a, _ := openTest(t)
	svg, err := RenderSVG(context.Background(), a, davutLat, davutLng, DefaultOptions)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`width="1280" height="480" viewBox="0 0 640 240"`, `aria-label="Etkinlik yeri haritası"`,
		`fill="` + colorEarth + `"`, `fill="` + colorGreen + `"`, `fill="` + colorCampus + `"`, `fill="` + colorBuilding + `"`,
		`stroke="#BFBFBF" stroke-width="7"`, `stroke="#FFFFFF" stroke-width="5.4"`, `stroke-dasharray="2 1.5"`,
		">Davutpaşa Caddesi</textPath>", ">Çiçek Sokağı</textPath>", ">Yıldız Teknik Üniversitesi</text>",
		`fill="` + colorPin + `"`, `<g transform="translate(320 120)">`,
	} {
		if !strings.Contains(svg, want) {
			t.Errorf("SVG has no %q", want)
		}
	}
	for _, unwanted := range []string{"Tünel Yolu", "<Sokak>", "http", "<script", "<image"} {
		if strings.Contains(strings.Replace(svg, `xmlns="http://www.w3.org/2000/svg"`, "", 1), unwanted) {
			t.Errorf("SVG has %q", unwanted)
		}
	}
	// A name along a road reads left to right.
	m := regexp.MustCompile(`<path id="y\d+" d="M([\d.-]+) [\d.-]+(?:L[\d.-]+ [\d.-]+)*L([\d.-]+) [\d.-]+"/>`).FindAllStringSubmatch(svg, -1)
	if len(m) == 0 {
		t.Fatal("no label path")
	}
	for _, path := range m {
		first, _ := strconv.ParseFloat(path[1], 64)
		last, _ := strconv.ParseFloat(path[2], 64)
		if last < first {
			t.Errorf("a name runs right to left: %s", path[0])
		}
	}
	if _, err := RenderSVG(context.Background(), a, 60, 10, DefaultOptions); !errors.Is(err, ErrNoMap) {
		t.Fatalf("outside the archive: %v", err)
	}
	if _, err := RenderSVG(context.Background(), a, 40.95, 28.85, DefaultOptions); !errors.Is(err, ErrNoMap) {
		t.Fatalf("inside its bounds, no tiles: %v", err)
	}
}

// fakeShot keeps the page it was given and answers a JPEG.
type fakeShot struct {
	page          string
	width, height int
	err           error
}

func (f *fakeShot) Screenshot(_ context.Context, html string, width, height, quality int) ([]byte, error) {
	f.page, f.width, f.height = html, width, height
	if f.err != nil {
		return nil, f.err
	}
	return []byte{0xff, 0xd8, 0xff}, nil
}

// The renderer opens the archive once, draws the SVG and takes its picture
// at twice the size; the page embeds its font and fetches nothing.
func TestRendererMap(t *testing.T) {
	t.Parallel()
	src := &bytesSource{b: testArchive(t)}
	shot := &fakeShot{}
	r := &Renderer{Source: src, Shot: shot}
	picture, err := r.Map(context.Background(), davutLat, davutLng)
	if err != nil || len(picture) != 3 || shot.width != 1280 || shot.height != 480 {
		t.Fatalf("%v %d×%d", err, shot.width, shot.height)
	}
	if !strings.Contains(shot.page, "@font-face{font-family:'Manrope'") || !strings.Contains(shot.page, "<svg ") ||
		strings.Contains(strings.ReplaceAll(shot.page, `xmlns="http://www.w3.org/2000/svg"`, ""), "http") {
		t.Fatal("the page fetches something, or lacks its font")
	}
	opened := r.archive
	if _, err := r.Map(context.Background(), davutLat, davutLng); err != nil || r.archive != opened {
		t.Fatalf("second map %v (archive opened again: %v)", err, r.archive != opened)
	}
	shot.err = errors.New("gotenberg down")
	if _, err := r.Map(context.Background(), davutLat, davutLng); !errors.Is(err, ErrShot) {
		t.Fatalf("a picture not taken: %v", err)
	}
	if _, err := r.Map(context.Background(), 60, 10); !errors.Is(err, ErrNoMap) {
		t.Fatalf("outside: %v", err)
	}
}

// The tiles are read by HTTP Range; a server that ignores it, or answers an
// error, is ErrUnavailable, and the error names neither the address nor
// the host.
func TestHTTPSource(t *testing.T) {
	t.Parallel()
	archive := testArchive(t)
	var ranges []string
	var mu sync.Mutex
	mode := "range"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ranges = append(ranges, r.Header.Get("Range"))
		m := mode
		mu.Unlock()
		switch m {
		case "whole":
			_, _ = w.Write(archive)
		case "missing":
			http.Error(w, "yok", http.StatusNotFound)
		default:
			http.ServeContent(w, r, "turkiye.pmtiles", fixedTime, bytes.NewReader(archive))
		}
	}))
	t.Cleanup(srv.Close)
	src := HTTPSource{URL: srv.URL + "/maps/turkiye.pmtiles", Client: srv.Client()}
	a, err := Open(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	if mu.Lock(); ranges[0] != "bytes=0-16383" {
		t.Errorf("first read %q", ranges[0])
	}
	mu.Unlock()
	x, y := tileOf(davutLat, davutLng, 15)
	if raw, err := a.Tile(context.Background(), 15, x, y); err != nil || raw == nil {
		t.Fatalf("tile %v", err)
	}
	for _, m := range []string{"whole", "missing"} {
		mu.Lock()
		mode = m
		mu.Unlock()
		_, err := src.ReadRange(context.Background(), 0, 100)
		if !errors.Is(err, ErrUnavailable) || strings.Contains(err.Error(), srv.URL) || strings.Contains(err.Error(), "127.0.0.1") {
			t.Errorf("%s: %v", m, err)
		}
	}
	srv.Close()
	if _, err := src.ReadRange(context.Background(), 0, 100); !errors.Is(err, ErrUnavailable) || strings.Contains(err.Error(), "127.0.0.1") || strings.Contains(err.Error(), "turkiye") {
		t.Errorf("a server gone: %v", err)
	}
}

var fixedTime = time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)

func TestTilesURLFromEnv(t *testing.T) {
	t.Parallel()
	for raw, ok := range map[string]bool{
		"": true, "https://cdn.example.test/maps/turkiye.pmtiles": true, "http://maptiles:8080/turkiye.pmtiles": true,
		"ftp://x/y": false, "https://user:pass@cdn.example.test/a.pmtiles": false, "turkiye.pmtiles": false, "https:///a": false,
	} {
		_, err := TilesURLFromEnv(func(string) string { return raw })
		if (err == nil) != ok {
			t.Errorf("%q: %v", raw, err)
		}
	}
}
