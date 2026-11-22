package mapimage

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
)

// Extracting a region (docs/event-map.md, "Regions"): the tiles of a box,
// up to a zoom, copied out of a larger archive into an archive of their
// own, as `pmtiles extract --bbox --maxzoom` does. The new archive's
// directories are known from the source's before a byte of tile data is
// read, so its header goes first and its tile data streams after, in tile
// order: no temporary file.

// BBox is a box in degrees.
type BBox struct {
	MinLon, MinLat, MaxLon, MaxLat float64
}

// Valid reports whether the box is a box on the Web Mercator map.
func (b BBox) Valid() bool {
	for _, v := range []float64{b.MinLon, b.MinLat, b.MaxLon, b.MaxLat} {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	return b.MinLon >= -180 && b.MaxLon <= 180 && b.MinLat >= -85.05112878 && b.MaxLat <= 85.05112878 &&
		b.MinLon < b.MaxLon && b.MinLat < b.MaxLat
}

// Contains reports whether the point is in the box.
func (b BBox) Contains(lat, lng float64) bool {
	return lat >= b.MinLat && lat <= b.MaxLat && lng >= b.MinLon && lng <= b.MaxLon
}

// Area is the box's area in square degrees: what "the smallest region
// holding a point" compares.
func (b BBox) Area() float64 {
	return (b.MaxLon - b.MinLon) * (b.MaxLat - b.MinLat)
}

// ErrTooLarge is an extract larger than the limit asked: refused before
// its tile data is read.
var ErrTooLarge = errors.New("mapimage: the extract is larger than allowed")

// rootLimit is how large the header and the root directory may be: the
// spec keeps them in the archive's first 16 KiB. A var, so a test can make
// leaf directories with few tiles.
var rootLimit = firstRead

// leafEntries is how many entries a leaf directory starts with; it doubles
// until the root fits.
const leafEntries = 4096

// copyRange is a run of the source's tile data the extract copies.
type copyRange struct {
	offset, length uint64
}

// ExtractPlan is an extract, decided: what it writes first (header,
// directories, metadata) and the source's tile data it copies after, in
// order.
type ExtractPlan struct {
	head   []byte
	copies []copyRange
	// Size is the extract's size in bytes; Tiles its tiles, Contents the
	// distinct tile data among them.
	Size            uint64
	Tiles, Contents uint64
}

// sourceTile is one tile of the box: its id and where its data is in the
// source (relative to the source's tile data).
type sourceTile struct {
	id             uint64
	offset, length uint64
}

// lookup finds the tile's data in the archive's directories, reading leaves
// into cache (the plan keeps every leaf it reads).
func (a *Archive) lookup(ctx context.Context, id uint64, cache map[uint64][]entry) (sourceTile, bool, error) {
	dir := a.root
	for depth := 0; depth < 4; depth++ {
		e, ok := find(dir, id)
		if !ok {
			return sourceTile{}, false, nil
		}
		if e.run > 0 {
			return sourceTile{id: id, offset: e.offset, length: e.length}, true, nil
		}
		leaf, ok := cache[e.offset]
		if !ok {
			raw, err := a.src.ReadRange(ctx, a.header.LeafOffset+e.offset, e.length)
			if err != nil {
				return sourceTile{}, false, err
			}
			if raw, err = decompress(raw, a.header.InternalCompression); err != nil {
				return sourceTile{}, false, err
			}
			if leaf, err = parseDirectory(raw); err != nil {
				return sourceTile{}, false, err
			}
			cache[e.offset] = leaf
		}
		dir = leaf
	}
	return sourceTile{}, false, ErrNotArchive
}

// tileRange is the tiles of zoom z the box touches.
func tileRange(box BBox, z int) (x0, y0, x1, y1 uint32) {
	last := math.Exp2(float64(z)) - 1
	clamp := func(v float64) uint32 { return uint32(math.Max(0, math.Min(last, math.Floor(v)))) }
	left, top := project(box.MaxLat, box.MinLon, z)
	right, bottom := project(box.MinLat, box.MaxLon, z)
	return clamp(left / 256), clamp(top / 256), clamp(right / 256), clamp(bottom / 256)
}

// PlanExtract decides the extract of the box up to maxZoom (no deeper than
// the source): every tile of each zoom the box touches, the source's runs
// and shared tile data kept, its metadata copied as it is (the basemap's
// attribution and licence go with the data). limit bounds its size (0 for
// none): ErrTooLarge beyond it.
func PlanExtract(ctx context.Context, a *Archive, box BBox, maxZoom int, limit uint64) (ExtractPlan, error) {
	if !box.Valid() {
		return ExtractPlan{}, fmt.Errorf("mapimage: not a box: %+v", box)
	}
	h := a.header
	maxZoom = min(maxZoom, int(h.MaxZoom))
	cache := map[uint64][]entry{}
	var tiles []sourceTile
	for z := int(h.MinZoom); z <= maxZoom; z++ {
		x0, y0, x1, y1 := tileRange(box, z)
		for x := x0; x <= x1; x++ {
			for y := y0; y <= y1; y++ {
				if err := ctx.Err(); err != nil {
					return ExtractPlan{}, err
				}
				t, ok, err := a.lookup(ctx, TileID(uint8(z), x, y), cache)
				if err != nil {
					return ExtractPlan{}, err
				}
				if ok {
					tiles = append(tiles, t)
				}
			}
		}
	}
	if len(tiles) == 0 {
		return ExtractPlan{}, ErrNoMap
	}
	slices.SortFunc(tiles, func(a, b sourceTile) int {
		switch {
		case a.id < b.id:
			return -1
		case a.id > b.id:
			return 1
		}
		return 0
	})

	// The new entries: a run while the same data follows on the next id;
	// data shared by several tiles is copied once (clustered: in the order
	// its first tile comes).
	placed := map[uint64]uint64{}
	var entries []entry
	var copies []copyRange
	var dataLength uint64
	for _, t := range tiles {
		out, seen := placed[t.offset]
		if !seen {
			out = dataLength
			placed[t.offset] = out
			dataLength += t.length
			copies = append(copies, copyRange{offset: h.TileDataOffset + t.offset, length: t.length})
		}
		if n := len(entries); n > 0 {
			last := &entries[n-1]
			if last.offset == out && last.tileID+uint64(last.run) == t.id && last.run < math.MaxUint32 {
				last.run++
				continue
			}
		}
		entries = append(entries, entry{tileID: t.id, offset: out, length: t.length, run: 1})
	}

	metadataRaw, err := a.metadataBytes(ctx)
	if err != nil {
		return ExtractPlan{}, err
	}
	metadata, err := gzipBytes(metadataRaw)
	if err != nil {
		return ExtractPlan{}, err
	}
	root, leaves, err := directories(entries)
	if err != nil {
		return ExtractPlan{}, err
	}
	head := make([]byte, headerLength, headerLength+len(root)+len(metadata)+len(leaves))
	copy(head, "PMTiles")
	head[7] = 3
	put := func(at int, v uint64) { binary.LittleEndian.PutUint64(head[at:], v) }
	rootOffset := uint64(headerLength)
	metadataOffset := rootOffset + uint64(len(root))
	leafOffset := metadataOffset + uint64(len(metadata))
	dataOffset := leafOffset + uint64(len(leaves))
	put(8, rootOffset)
	put(16, uint64(len(root)))
	put(24, metadataOffset)
	put(32, uint64(len(metadata)))
	put(40, leafOffset)
	put(48, uint64(len(leaves)))
	put(56, dataOffset)
	put(64, dataLength)
	put(72, uint64(len(tiles)))
	put(80, uint64(len(entries)))
	put(88, uint64(len(copies)))
	head[96] = 1
	head[97], head[98], head[99] = compressionGzip, h.TileCompression, tileTypeMVT
	head[100], head[101] = h.MinZoom, uint8(maxZoom)
	bounds := BBox{math.Max(box.MinLon, h.MinLon), math.Max(box.MinLat, h.MinLat), math.Min(box.MaxLon, h.MaxLon), math.Min(box.MaxLat, h.MaxLat)}
	degrees := func(at int, v float64) {
		binary.LittleEndian.PutUint32(head[at:], uint32(int32(math.Round(v*1e7))))
	}
	degrees(102, bounds.MinLon)
	degrees(106, bounds.MinLat)
	degrees(110, bounds.MaxLon)
	degrees(114, bounds.MaxLat)
	head[118] = uint8(max(int(h.MinZoom), maxZoom-3))
	degrees(119, (bounds.MinLon+bounds.MaxLon)/2)
	degrees(123, (bounds.MinLat+bounds.MaxLat)/2)
	head = append(head, root...)
	head = append(head, metadata...)
	head = append(head, leaves...)
	plan := ExtractPlan{head: head, copies: copies, Size: dataOffset + dataLength, Tiles: uint64(len(tiles)), Contents: uint64(len(copies))}
	if limit > 0 && plan.Size > limit {
		return plan, fmt.Errorf("%w: %d bytes, the limit is %d", ErrTooLarge, plan.Size, limit)
	}
	return plan, nil
}

// metadataBytes is the archive's metadata JSON as it is stored, opened.
func (a *Archive) metadataBytes(ctx context.Context) ([]byte, error) {
	h := a.header
	if h.MetadataLength == 0 {
		return []byte("{}"), nil
	}
	if h.MetadataLength > maxMetadata {
		return nil, fmt.Errorf("%w: metadata of %d bytes", ErrNotArchive, h.MetadataLength)
	}
	raw, err := a.src.ReadRange(ctx, h.MetadataOffset, h.MetadataLength)
	if err != nil {
		return nil, err
	}
	return decompress(raw, h.InternalCompression)
}

// serializeDirectory writes entries as the spec's directory: ids as
// deltas, then runs, lengths and offsets (0 for "right after the last").
func serializeDirectory(entries []entry) []byte {
	b := binary.AppendUvarint(nil, uint64(len(entries)))
	var last uint64
	for _, e := range entries {
		b = binary.AppendUvarint(b, e.tileID-last)
		last = e.tileID
	}
	for _, e := range entries {
		b = binary.AppendUvarint(b, uint64(e.run))
	}
	for _, e := range entries {
		b = binary.AppendUvarint(b, e.length)
	}
	for i, e := range entries {
		if i > 0 && e.offset == entries[i-1].offset+entries[i-1].length {
			b = binary.AppendUvarint(b, 0)
		} else {
			b = binary.AppendUvarint(b, e.offset+1)
		}
	}
	return b
}

// directories is the root directory, and the leaves when every entry does
// not fit the root: leaves of leafEntries entries, doubled until the root
// that lists them fits.
func directories(entries []entry) (root, leaves []byte, err error) {
	if root, err = gzipBytes(serializeDirectory(entries)); err != nil {
		return nil, nil, err
	}
	if headerLength+len(root) <= rootLimit {
		return root, nil, nil
	}
	for size := leafEntries; ; size *= 2 {
		var rootEntries []entry
		leaves = leaves[:0]
		for start := 0; start < len(entries); start += size {
			chunk := entries[start:min(start+size, len(entries))]
			leaf, err := gzipBytes(serializeDirectory(chunk))
			if err != nil {
				return nil, nil, err
			}
			rootEntries = append(rootEntries, entry{tileID: chunk[0].tileID, offset: uint64(len(leaves)), length: uint64(len(leaf))})
			leaves = append(leaves, leaf...)
		}
		if root, err = gzipBytes(serializeDirectory(rootEntries)); err != nil {
			return nil, nil, err
		}
		if headerLength+len(root) <= rootLimit || size >= len(entries) {
			return root, leaves, nil
		}
	}
}

// maxGap is how many unneeded bytes between two tiles a read takes along
// rather than asking again: fewer, larger range requests.
const maxGap = 256 << 10

// WriteTo writes the extract: what goes first, then the source's tile data
// in order, read in ranges of up to maxRange that run over small gaps.
// progress, when not nil, is told the bytes written so far.
func (p ExtractPlan) WriteTo(ctx context.Context, src RangeSource, w io.Writer, progress func(written uint64)) error {
	if _, err := w.Write(p.head); err != nil {
		return err
	}
	written := uint64(len(p.head))
	for i := 0; i < len(p.copies); {
		start := p.copies[i].offset
		end := start + p.copies[i].length
		j := i + 1
		for j < len(p.copies) {
			next := p.copies[j]
			if next.offset < end || next.offset-end > maxGap || next.offset+next.length-start > maxRange {
				break
			}
			end = next.offset + next.length
			j++
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		data, err := src.ReadRange(ctx, start, end-start)
		if err != nil {
			return err
		}
		if uint64(len(data)) != end-start {
			return fmt.Errorf("%w: a short read of the source's tiles", ErrUnavailable)
		}
		for _, c := range p.copies[i:j] {
			if _, err := w.Write(data[c.offset-start : c.offset-start+c.length]); err != nil {
				return err
			}
			written += c.length
		}
		if progress != nil {
			progress(written)
		}
		i = j
	}
	return nil
}
