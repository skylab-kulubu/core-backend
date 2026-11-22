// Package mapimage draws an Event's place as a map image, once per point
// (docs/event-map.md): core reads the few vector tiles around the point
// from the club's own PMTiles archive of OpenStreetMap data (a Protomaps
// basemap extract), draws them as an SVG in the ticket's grey, with the
// club's pin, and has Gotenberg's Chromium take a picture of it. No map
// service, no key, and the renderer reaches nothing outside the page.
package mapimage

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"
)

// ErrNotArchive is a file that is not a PMTiles v3 archive of vector tiles
// core can read.
var ErrNotArchive = errors.New("mapimage: not a PMTiles v3 archive of gzip'd or plain vector tiles")

// ErrUnavailable is an archive that cannot be read now (the server does not
// answer, or answers an error): the map waits and is tried again.
var ErrUnavailable = errors.New("mapimage: the tile archive cannot be read")

// RangeSource reads a byte range of the archive.
type RangeSource interface {
	ReadRange(ctx context.Context, offset, length uint64) ([]byte, error)
}

// maxRange bounds one read: a directory or a tile is far smaller.
const maxRange = 8 << 20

// HTTPSource reads the archive at URL with HTTP range requests (an object
// in the public bucket, or any static server that answers Range).
type HTTPSource struct {
	URL    string
	Client *http.Client
}

func (h HTTPSource) ReadRange(ctx context.Context, offset, length uint64) ([]byte, error) {
	if length == 0 || length > maxRange {
		return nil, fmt.Errorf("%w: a read of %d bytes", ErrNotArchive, length)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.URL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Range", "bytes="+strconv.FormatUint(offset, 10)+"-"+strconv.FormatUint(offset+length-1, 10))
	client := h.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		// Only the kind of failure: the error names the URL and the
		// address it reached.
		var timeout interface{ Timeout() bool }
		if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &timeout) && timeout.Timeout()) {
			return nil, fmt.Errorf("%w: the request timed out", ErrUnavailable)
		}
		return nil, fmt.Errorf("%w: the request failed", ErrUnavailable)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusPartialContent:
	case http.StatusOK:
		// A server that ignores Range sends the whole file: refused, as
		// the archive is far too large to read whole.
		return nil, fmt.Errorf("%w: the server ignores Range requests", ErrUnavailable)
	default:
		return nil, fmt.Errorf("%w: HTTP %d", ErrUnavailable, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(length)+1))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if uint64(len(body)) > length {
		return nil, fmt.Errorf("%w: a longer answer than asked", ErrUnavailable)
	}
	return body, nil
}

// Header is the archive's header (PMTiles v3, 127 bytes).
type Header struct {
	RootOffset, RootLength         uint64
	LeafOffset, LeafLength         uint64
	TileDataOffset, TileDataLength uint64
	InternalCompression            uint8
	TileCompression                uint8
	TileType                       uint8
	MinZoom, MaxZoom               uint8
	// The archive's bounds, in degrees.
	MinLon, MinLat, MaxLon, MaxLat float64
}

// The header's compression and tile type codes.
const (
	compressionNone = 1
	compressionGzip = 2
	tileTypeMVT     = 1
	headerLength    = 127
	// firstRead is what Open reads at once: the header and, in a
	// clustered archive, the root directory.
	firstRead = 16384
)

func parseHeader(b []byte) (Header, error) {
	if len(b) < headerLength || string(b[:7]) != "PMTiles" || b[7] != 3 {
		return Header{}, ErrNotArchive
	}
	u := func(at int) uint64 { return binary.LittleEndian.Uint64(b[at:]) }
	deg := func(at int) float64 { return float64(int32(binary.LittleEndian.Uint32(b[at:]))) / 1e7 }
	h := Header{
		RootOffset: u(8), RootLength: u(16), LeafOffset: u(40), LeafLength: u(48),
		TileDataOffset: u(56), TileDataLength: u(64),
		InternalCompression: b[97], TileCompression: b[98], TileType: b[99], MinZoom: b[100], MaxZoom: b[101],
		MinLon: deg(102), MinLat: deg(106), MaxLon: deg(110), MaxLat: deg(114),
	}
	if h.TileType != tileTypeMVT || !knownCompression(h.InternalCompression) || !knownCompression(h.TileCompression) || h.MaxZoom < h.MinZoom {
		return Header{}, ErrNotArchive
	}
	return h, nil
}

func knownCompression(c uint8) bool { return c == compressionNone || c == compressionGzip }

// entry is one directory entry: a run of tiles stored once, or (run 0) a
// leaf directory.
type entry struct {
	tileID, offset, length uint64
	run                    uint32
}

func decompress(b []byte, compression uint8) ([]byte, error) {
	if compression != compressionGzip {
		return b, nil
	}
	r, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotArchive, err)
	}
	out, err := io.ReadAll(io.LimitReader(r, maxRange+1))
	if err != nil || len(out) > maxRange {
		return nil, fmt.Errorf("%w: a compressed part does not open", ErrNotArchive)
	}
	return out, nil
}

func parseDirectory(b []byte) ([]entry, error) {
	r := bytes.NewReader(b)
	n, err := binary.ReadUvarint(r)
	if err != nil || n > uint64(len(b)) {
		return nil, ErrNotArchive
	}
	entries := make([]entry, n)
	var last uint64
	for i := range entries {
		v, err := binary.ReadUvarint(r)
		if err != nil {
			return nil, ErrNotArchive
		}
		last += v
		entries[i].tileID = last
	}
	for i := range entries {
		v, err := binary.ReadUvarint(r)
		if err != nil || v > 1<<32-1 {
			return nil, ErrNotArchive
		}
		entries[i].run = uint32(v)
	}
	for i := range entries {
		v, err := binary.ReadUvarint(r)
		if err != nil {
			return nil, ErrNotArchive
		}
		entries[i].length = v
	}
	for i := range entries {
		v, err := binary.ReadUvarint(r)
		if err != nil {
			return nil, ErrNotArchive
		}
		if v == 0 && i > 0 {
			entries[i].offset = entries[i-1].offset + entries[i-1].length
		} else {
			entries[i].offset = v - 1
		}
	}
	return entries, nil
}

// find is the entry that holds id: the last one at or before it, when its
// run reaches id or it is a leaf.
func find(entries []entry, id uint64) (entry, bool) {
	i := sort.Search(len(entries), func(i int) bool { return entries[i].tileID > id }) - 1
	if i < 0 {
		return entry{}, false
	}
	e := entries[i]
	if e.run == 0 || id < e.tileID+uint64(e.run) {
		return e, true
	}
	return entry{}, false
}

// TileID is the tile's id in the archive: the tiles of the zooms before
// it, then its place on the zoom's Hilbert curve (the PMTiles spec).
func TileID(z uint8, x, y uint32) uint64 {
	acc := (uint64(1)<<(2*uint64(z)) - 1) / 3
	if z == 0 {
		return 0
	}
	n := uint32(z - 1)
	for s := uint32(1) << n; s > 0; s >>= 1 {
		rx, ry := s&x, s&y
		acc += uint64((3*rx)^ry) << n
		if ry == 0 {
			if rx != 0 {
				x, y = s-1-x, s-1-y
			}
			x, y = y, x
		}
		n--
	}
	return acc
}

// Archive reads tiles from a PMTiles archive. It keeps the root directory
// and the last leaf directories it read.
type Archive struct {
	src    RangeSource
	header Header
	root   []entry

	mu     sync.Mutex
	leaves map[uint64][]entry
}

// maxLeaves bounds the leaf directories kept.
const maxLeaves = 32

// Open reads the archive's header and root directory.
func Open(ctx context.Context, src RangeSource) (*Archive, error) {
	head, err := src.ReadRange(ctx, 0, firstRead)
	if err != nil {
		return nil, err
	}
	h, err := parseHeader(head)
	if err != nil {
		return nil, err
	}
	var raw []byte
	if h.RootOffset+h.RootLength <= uint64(len(head)) {
		raw = head[h.RootOffset : h.RootOffset+h.RootLength]
	} else if raw, err = src.ReadRange(ctx, h.RootOffset, h.RootLength); err != nil {
		return nil, err
	}
	raw, err = decompress(raw, h.InternalCompression)
	if err != nil {
		return nil, err
	}
	root, err := parseDirectory(raw)
	if err != nil {
		return nil, err
	}
	return &Archive{src: src, header: h, root: root, leaves: map[uint64][]entry{}}, nil
}

// Header is the archive's header.
func (a *Archive) Header() Header { return a.header }

// Covers reports whether the archive's bounds hold the point.
func (a *Archive) Covers(lat, lng float64) bool {
	h := a.header
	return lat >= h.MinLat && lat <= h.MaxLat && lng >= h.MinLon && lng <= h.MaxLon
}

func (a *Archive) leaf(ctx context.Context, e entry) ([]entry, error) {
	a.mu.Lock()
	cached, ok := a.leaves[e.offset]
	a.mu.Unlock()
	if ok {
		return cached, nil
	}
	raw, err := a.src.ReadRange(ctx, a.header.LeafOffset+e.offset, e.length)
	if err != nil {
		return nil, err
	}
	raw, err = decompress(raw, a.header.InternalCompression)
	if err != nil {
		return nil, err
	}
	entries, err := parseDirectory(raw)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	if len(a.leaves) >= maxLeaves {
		clear(a.leaves)
	}
	a.leaves[e.offset] = entries
	a.mu.Unlock()
	return entries, nil
}

// Tile is the tile's vector data, uncompressed; nil when the archive has no
// such tile (the sea, or outside its extract).
func (a *Archive) Tile(ctx context.Context, z uint8, x, y uint32) ([]byte, error) {
	if z < a.header.MinZoom || z > a.header.MaxZoom {
		return nil, nil
	}
	id := TileID(z, x, y)
	dir := a.root
	// A directory tree is at most four deep (the spec's root and leaves).
	for depth := 0; depth < 4; depth++ {
		e, ok := find(dir, id)
		if !ok {
			return nil, nil
		}
		if e.run > 0 {
			raw, err := a.src.ReadRange(ctx, a.header.TileDataOffset+e.offset, e.length)
			if err != nil {
				return nil, err
			}
			return decompress(raw, a.header.TileCompression)
		}
		leaf, err := a.leaf(ctx, e)
		if err != nil {
			return nil, err
		}
		dir = leaf
	}
	return nil, ErrNotArchive
}
