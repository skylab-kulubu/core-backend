package mapimage

import (
	"bytes"
	"context"
	"errors"
	"math"
	"testing"
)

// The test archive's center tile at zoom 15 and the box just inside it.
func centerBox(t *testing.T) (BBox, uint32, uint32) {
	t.Helper()
	x, y := tileOf(davutLat, davutLng, 15)
	// The tile's corners, pulled in a little.
	toLon := func(px float64) float64 { return px/(256*32768)*360 - 180 }
	toLat := func(py float64) float64 {
		n := math.Pi - 2*math.Pi*py/(256*32768)
		return 180 / math.Pi * math.Atan(math.Sinh(n))
	}
	box := BBox{
		MinLon: toLon(float64(x)*256 + 8), MaxLon: toLon(float64(x+1)*256 - 8),
		MaxLat: toLat(float64(y)*256 + 8), MinLat: toLat(float64(y+1)*256 - 8),
	}
	return box, x, y
}

// An extract holds the box's tiles, byte for byte, and nothing else; it
// keeps the source's metadata, and opens as an archive of its own.
func TestExtractKeepsTheBoxsTiles(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	source, _ := openTest(t)
	box, x, y := centerBox(t)
	plan, err := PlanExtract(ctx, source, box, 15, 0)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Tiles != 1 || plan.Contents != 1 {
		t.Fatalf("plan %d tiles, %d contents", plan.Tiles, plan.Contents)
	}
	var out bytes.Buffer
	var progress uint64
	if err := plan.WriteTo(ctx, &bytesSource{b: testArchive(t)}, &out, func(n uint64) { progress = n }); err != nil {
		t.Fatal(err)
	}
	if uint64(out.Len()) != plan.Size || progress != plan.Size {
		t.Fatalf("wrote %d (progress %d), planned %d", out.Len(), progress, plan.Size)
	}
	extract, err := Open(ctx, &bytesSource{b: out.Bytes()})
	if err != nil {
		t.Fatal(err)
	}
	want, _ := source.Tile(ctx, 15, x, y)
	if got, err := extract.Tile(ctx, 15, x, y); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("the box's tile: %d bytes, %v", len(got), err)
	}
	if got, err := extract.Tile(ctx, 15, x+1, y); err != nil || got != nil {
		t.Fatalf("a tile outside the box: %d bytes, %v", len(got), err)
	}
	h := extract.Header()
	if h.MaxZoom != 15 || h.MinLon < box.MinLon-1e-6 || h.MaxLat > box.MaxLat+1e-6 {
		t.Fatalf("header %+v", h)
	}
	if metadata, err := extract.Metadata(ctx); err != nil || metadata["version"] != "4.15.2" {
		t.Fatalf("metadata %v %v", metadata, err)
	}
	if _, err := RenderSVG(ctx, extract, davutLat, davutLng, DefaultOptions); err != nil {
		t.Fatalf("the extract does not draw: %v", err)
	}
}

// A box of the whole test archive: every tile, the data the eight plain
// tiles share stored once, and a limit below its size refused before any
// tile data is read.
func TestExtractSharesDataAndKeepsItsLimit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	source, _ := openTest(t)
	whole := BBox{MinLon: 28.8, MinLat: 40.9, MaxLon: 29.1, MaxLat: 41.2}
	plan, err := PlanExtract(ctx, source, whole, 15, 0)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Tiles != 9 || plan.Contents != 2 {
		t.Fatalf("plan %d tiles, %d contents", plan.Tiles, plan.Contents)
	}
	if _, err := PlanExtract(ctx, source, whole, 15, plan.Size-1); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("over the limit: %v", err)
	}
	if _, err := PlanExtract(ctx, source, BBox{MinLon: 10, MinLat: 10, MaxLon: 11, MaxLat: 11}, 15, 0); !errors.Is(err, ErrNoMap) {
		t.Fatalf("a box with no tiles: %v", err)
	}
	if _, err := PlanExtract(ctx, source, BBox{MinLon: 29, MinLat: 41, MaxLon: 28, MaxLat: 42}, 15, 0); err == nil {
		t.Fatal("not a box")
	}
}

// With more entries than the root holds, the extract writes leaf
// directories, and reads back the same.
func TestExtractWritesLeafDirectories(t *testing.T) {
	saved := rootLimit
	rootLimit = headerLength + 40
	t.Cleanup(func() { rootLimit = saved })
	ctx := context.Background()
	source, _ := openTest(t)
	whole := BBox{MinLon: 28.8, MinLat: 40.9, MaxLon: 29.1, MaxLat: 41.2}
	entries := make([]entry, 0, 9000)
	for i := range 9000 {
		entries = append(entries, entry{tileID: uint64(100 + 2*i), offset: uint64(i), length: 1, run: 1})
	}
	root, leaves, err := directories(entries)
	if err != nil || len(leaves) == 0 {
		t.Fatalf("no leaves: %v", err)
	}
	raw, _ := decompress(root, compressionGzip)
	if listed, err := parseDirectory(raw); err != nil || len(listed) < 2 || listed[0].run != 0 {
		t.Fatalf("root %+v %v", listed, err)
	}
	plan, err := PlanExtract(ctx, source, whole, 15, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := plan.WriteTo(ctx, &bytesSource{b: testArchive(t)}, &out, nil); err != nil {
		t.Fatal(err)
	}
	extract, err := Open(ctx, &bytesSource{b: out.Bytes()})
	if err != nil {
		t.Fatal(err)
	}
	x, y := tileOf(davutLat, davutLng, 15)
	for dx := uint32(0); dx < 3; dx++ {
		for dy := uint32(0); dy < 3; dy++ {
			want, _ := source.Tile(ctx, 15, x-1+dx, y-1+dy)
			if got, err := extract.Tile(ctx, 15, x-1+dx, y-1+dy); err != nil || !bytes.Equal(got, want) {
				t.Fatalf("tile %d,%d: %v", dx, dy, err)
			}
		}
	}
}
