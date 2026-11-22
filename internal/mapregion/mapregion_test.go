package mapregion

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/mapimage"
	"github.com/skylab-kulubu/core-backend/internal/mapimage/maptest"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

const davutLat, davutLng = 41.025376, 28.889648

// memory serves an archive from memory.
type memory struct{ b []byte }

func (m memory) ReadRange(_ context.Context, offset, length uint64) ([]byte, error) {
	if offset >= uint64(len(m.b)) {
		return nil, mapimage.ErrUnavailable
	}
	return m.b[offset:min(offset+length, uint64(len(m.b)))], nil
}

type shot struct{}

func (shot) Screenshot(context.Context, string, int, int, int) ([]byte, error) {
	return []byte{0xff, 0xd8, 0xff}, nil
}

type lister struct {
	mu    sync.Mutex
	lists [][]Region
	reads int
}

func (l *lister) ListReady(context.Context) ([]Region, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.reads++
	if len(l.lists) > 1 {
		head := l.lists[0]
		l.lists = l.lists[1:]
		return head, nil
	}
	return l.lists[0], nil
}

func region(name string, box mapimage.BBox) Region {
	return Region{ID: uuid.New(), Name: name, Box: box, Status: StatusReady, ObjectKey: "maps/" + name + ".pmtiles"}
}

var (
	istanbul = mapimage.BBox{MinLon: 27.95, MinLat: 40.80, MaxLon: 29.95, MaxLat: 41.60}
	turkiye  = mapimage.BBox{MinLon: 25.66, MinLat: 35.81, MaxLon: 44.83, MaxLat: 42.11}
)

// A point is drawn from the smallest ready region that holds it; with no
// region at all nothing can be drawn yet (no try counted); with regions
// none of which holds it there is no map, unless MAP_TILES_URL's archive
// is there to fall back to. A list kept from before a region was ready is
// read again before a point is refused.
func TestRegistryPicksTheSmallestRegion(t *testing.T) {
	t.Parallel()
	archive := maptest.City(davutLat, davutLng, 1, 27.95, 40.80, 29.95, 41.60, maptest.Metadata)
	var opened []string
	var mu sync.Mutex
	source := func(url string) mapimage.RangeSource {
		mu.Lock()
		opened = append(opened, url)
		mu.Unlock()
		return memory{b: archive}
	}
	address := func(key string) string { return "https://cdn.example.test/" + key }
	ctx := context.Background()

	empty := &Registry{Regions: &lister{lists: [][]Region{{}}}, Shot: shot{}, Address: address, Source: source}
	if _, err := empty.Map(ctx, davutLat, davutLng); !errors.Is(err, mapimage.ErrUnavailable) {
		t.Fatalf("no region: %v", err)
	}

	both := &Registry{Regions: &lister{lists: [][]Region{{region("istanbul", istanbul), region("turkiye", turkiye)}}}, Shot: shot{}, Address: address, Source: source}
	if _, err := both.Map(ctx, davutLat, davutLng); err != nil {
		t.Fatal(err)
	}
	if len(opened) != 1 || opened[0] != "https://cdn.example.test/maps/istanbul.pmtiles" {
		t.Fatalf("opened %v", opened)
	}
	if _, err := both.Map(ctx, 39.9, 32.85); err == nil || errors.Is(err, mapimage.ErrUnavailable) {
		// Ankara is in Türkiye's box: drawn from it (the test archive has
		// no tile there, so no map, but from the right archive).
		t.Logf("Ankara: %v", err)
	}
	if opened[len(opened)-1] != "https://cdn.example.test/maps/turkiye.pmtiles" {
		t.Fatalf("Ankara from %v", opened)
	}
	if _, err := both.Map(ctx, 48.85, 2.35); !errors.Is(err, mapimage.ErrNoMap) {
		t.Fatalf("Paris: %v", err)
	}
	withStatic := &Registry{Regions: &lister{lists: [][]Region{{region("turkiye", turkiye)}}}, Shot: shot{}, Address: address, Source: source,
		Static: "https://tiles.example.test/world.pmtiles"}
	_, _ = withStatic.Map(ctx, 48.85, 2.35)
	if opened[len(opened)-1] != "https://tiles.example.test/world.pmtiles" {
		t.Fatalf("Paris from %v", opened)
	}

	stale := &lister{lists: [][]Region{{region("turkiye", mapimage.BBox{MinLon: 30, MinLat: 36, MaxLon: 44, MaxLat: 42})}, {region("istanbul", istanbul)}}}
	late := &Registry{Regions: stale, Shot: shot{}, Address: address, Source: source}
	if _, err := late.Map(ctx, davutLat, davutLng); err != nil || stale.reads != 2 {
		t.Fatalf("a region ready since the list was read: %v (%d reads)", err, stale.reads)
	}
}

// The latest build of the basemap's version 4, by its day.
func TestBuildsLatest(t *testing.T) {
	t.Parallel()
	answer := `[{"key":"20261120.pmtiles","version":"4.15.2"},{"key":"20261123.pmtiles","version":"5.0.0"},{"key":"notes.txt","version":"4.1"},{"key":"20261122.pmtiles","version":"4.15.2"},{"key":"20261121.pmtiles","version":"4.15.2"}]`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(answer))
	}))
	t.Cleanup(srv.Close)
	build, err := Builds{ListURL: srv.URL, Base: "https://build.example.test/", Client: srv.Client()}.Latest(context.Background())
	if err != nil || build.Date != "20261122" || build.URL != "https://build.example.test/20261122.pmtiles" || build.Version != "4.15.2" {
		t.Fatalf("%+v %v", build, err)
	}
	answer = `[{"key":"20261123.pmtiles","version":"5.0.0"}]`
	if _, err := (Builds{ListURL: srv.URL, Client: srv.Client()}).Latest(context.Background()); !errors.Is(err, ErrNoBuild) {
		t.Fatalf("no version 4: %v", err)
	}
}

// memoryUploader keeps a multipart upload's parts.
type memoryUploader struct {
	mu       sync.Mutex
	parts    map[int32][]byte
	objects  map[string][]byte
	aborted  []string
	failPart int32
}

func newMemoryUploader() *memoryUploader {
	return &memoryUploader{parts: map[int32][]byte{}, objects: map[string][]byte{}}
}

func (u *memoryUploader) CreateMultipartWith(_ context.Context, key string, meta media.BlobMetadata) (string, error) {
	if meta.ContentType != "application/octet-stream" {
		return "", errors.New("wrong type")
	}
	return "upload-" + key, nil
}

func (u *memoryUploader) UploadPart(_ context.Context, _, _ string, number int32, data []byte) (string, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if number == u.failPart {
		return "", errors.New("storage down")
	}
	u.parts[number] = append([]byte(nil), data...)
	return "etag", nil
}

func (u *memoryUploader) CompleteMultipart(_ context.Context, key, _ string, parts []media.UploadedPart) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	var all bytes.Buffer
	for i, p := range parts {
		if p.Number != int32(i+1) || (i < len(parts)-1 && p.Size != partSize) {
			return errors.New("parts out of shape")
		}
		all.Write(u.parts[p.Number])
	}
	u.objects[key] = all.Bytes()
	return nil
}

func (u *memoryUploader) AbortMultipartUploads(_ context.Context, key string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.aborted = append(u.aborted, key)
	return nil
}

// The parts are all of partSize but the last, numbered from 1, and add up
// to what was written.
func TestPartsAreEqualButTheLast(t *testing.T) {
	t.Parallel()
	u := newMemoryUploader()
	var sent []int64
	p := &parts{ctx: context.Background(), uploader: u, key: "maps/x.pmtiles", id: "1", after: func(n int64) error { sent = append(sent, n); return nil }}
	data := bytes.Repeat([]byte("harita"), (2*partSize+1234)/6+1)
	for chunk := data; len(chunk) > 0; {
		n := min(len(chunk), 3<<20)
		if _, err := p.Write(chunk[:n]); err != nil {
			t.Fatal(err)
		}
		chunk = chunk[n:]
	}
	if err := p.close(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(u.objects["maps/x.pmtiles"], data) || len(sent) != 3 || sent[2] != int64(len(data)) {
		t.Fatalf("object %d bytes (want %d), progress %v", len(u.objects["maps/x.pmtiles"]), len(data), sent)
	}
}

// A request's name, box and zoom; the extract's key.
func TestRequestsAndKeys(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		name string
		box  mapimage.BBox
		zoom int
		ok   bool
	}{
		"İstanbul":        {"İstanbul", istanbul, 15, true},
		"Türkiye":         {"Türkiye", turkiye, 15, true},
		"no name":         {"  ", istanbul, 15, false},
		"a long name":     {strings.Repeat("a", NameMax+1), istanbul, 15, false},
		"upside down":     {"x", mapimage.BBox{MinLon: 29, MinLat: 41, MaxLon: 28, MaxLat: 42}, 15, false},
		"the world":       {"x", mapimage.BBox{MinLon: -180, MinLat: -85, MaxLon: 180, MaxLat: 85}, 15, false},
		"too deep a zoom": {"x", istanbul, 16, false},
	} {
		if validRequest(tc.name, tc.box, tc.zoom) != tc.ok {
			t.Errorf("%s: want %v", name, tc.ok)
		}
	}
	r := Region{ID: uuid.MustParse("12345678-1234-1234-1234-123456789abc"), Name: "İstanbul Avrupa Yakası"}
	if key := ObjectKeyFor(r, "20261122"); key != "maps/istanbul-avrupa-yakasi-20261122-12345678.pmtiles" {
		t.Fatalf("key %q", key)
	}
}
