// Package maptest writes small PMTiles archives of vector tiles for tests
// of the packages that draw or extract maps: the spec's layout, gzip'd, one
// root directory, Protomaps basemap 4 metadata.
package maptest

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"math"
	"sort"
)

// TileID is the tile's PMTiles id (the spec's Hilbert order).
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

// TileOf is the zoom z tile that holds the point.
func TileOf(lat, lng float64, z uint8) (uint32, uint32) {
	n := math.Exp2(float64(z))
	phi := lat * math.Pi / 180
	return uint32((lng + 180) / 360 * n), uint32((1 - math.Log(math.Tan(phi)+1/math.Cos(phi))/math.Pi) / 2 * n)
}

type pbuf struct{ b []byte }

func (p *pbuf) varint(v uint64)       { p.b = binary.AppendUvarint(p.b, v) }
func (p *pbuf) key(field, typ uint64) { p.varint(field<<3 | typ) }
func (p *pbuf) bytes(f uint64, b []byte) {
	p.key(f, 2)
	p.varint(uint64(len(b)))
	p.b = append(p.b, b...)
}
func (p *pbuf) uint(f, v uint64) { p.key(f, 0); p.varint(v) }

func zz(v int) uint32 { return uint32((v << 1) ^ (v >> 31)) }

// box is a closed square ring, in tile units.
func box(x0, y0, x1, y1 int) [][2]int {
	return [][2]int{{x0, y0}, {x1, y0}, {x1, y1}, {x0, y1}, {x0, y0}}
}

func polygon(ring [][2]int) []byte {
	var p pbuf
	p.varint(1 | 1<<3)
	p.varint(uint64(zz(ring[0][0])))
	p.varint(uint64(zz(ring[0][1])))
	p.varint(uint64(2 | uint32(len(ring)-2)<<3))
	x, y := ring[0][0], ring[0][1]
	for _, pt := range ring[1 : len(ring)-1] {
		p.varint(uint64(zz(pt[0] - x)))
		p.varint(uint64(zz(pt[1] - y)))
		x, y = pt[0], pt[1]
	}
	p.varint(7 | 1<<3)
	return p.b
}

func line(points [][2]int) []byte {
	var p pbuf
	p.varint(1 | 1<<3)
	p.varint(uint64(zz(points[0][0])))
	p.varint(uint64(zz(points[0][1])))
	p.varint(uint64(2 | uint32(len(points)-1)<<3))
	x, y := points[0][0], points[0][1]
	for _, pt := range points[1:] {
		p.varint(uint64(zz(pt[0] - x)))
		p.varint(uint64(zz(pt[1] - y)))
		x, y = pt[0], pt[1]
	}
	return p.b
}

// layer is one layer with one feature of the kind, its geometry and props.
func layer(name string, kind uint64, geometry []byte, props map[string]string) []byte {
	var l pbuf
	l.uint(15, 2)
	l.bytes(1, []byte(name))
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var f, tags pbuf
	for i := range keys {
		tags.varint(uint64(i))
		tags.varint(uint64(i))
	}
	f.bytes(2, tags.b)
	f.uint(3, kind)
	f.bytes(4, geometry)
	l.bytes(2, f.b)
	for _, k := range keys {
		l.bytes(3, []byte(k))
	}
	for _, k := range keys {
		var v pbuf
		v.bytes(1, []byte(props[k]))
		l.bytes(4, v.b)
	}
	l.uint(5, 4096)
	return l.b
}

// cityTile is a tile with land, a building and a named road through its
// middle: what a city point's map needs.
func cityTile() []byte {
	var t pbuf
	t.bytes(3, layer("earth", 3, polygon(box(-64, -64, 4160, 4160)), map[string]string{"kind": "earth"}))
	t.bytes(3, layer("buildings", 3, polygon(box(1800, 1800, 2200, 2200)), map[string]string{"kind": "building"}))
	t.bytes(3, layer("roads", 2, line([][2]int{{-64, 2048}, {4160, 2048}}), map[string]string{"kind": "major_road", "name": "Davutpaşa Caddesi"}))
	return t.b
}

func gz(b []byte) []byte {
	var out bytes.Buffer
	w := gzip.NewWriter(&out)
	_, _ = w.Write(b)
	_ = w.Close()
	return out.Bytes()
}

// Metadata is a Protomaps basemap 4's metadata.
const Metadata = `{"name":"Protomaps Basemap","version":"4.15.2","attribution":"© OpenStreetMap"}`

// City is an archive of zoom 15 tiles around the point, radius tiles each
// way, every one a city tile, within the bounds given, with the metadata
// given (Metadata for a Protomaps basemap 4).
func City(lat, lng float64, radius uint32, minLon, minLat, maxLon, maxLat float64, metadata string) []byte {
	x, y := TileOf(lat, lng, 15)
	type tile struct {
		id   uint64
		data []byte
	}
	data := gz(cityTile())
	var tiles []tile
	for dx := x - radius; dx <= x+radius; dx++ {
		for dy := y - radius; dy <= y+radius; dy++ {
			tiles = append(tiles, tile{id: TileID(15, dx, dy), data: data})
		}
	}
	sort.Slice(tiles, func(i, j int) bool { return tiles[i].id < tiles[j].id })
	var dir pbuf
	dir.varint(uint64(len(tiles)))
	var last uint64
	for _, t := range tiles {
		dir.varint(t.id - last)
		last = t.id
	}
	for range tiles {
		dir.varint(1)
	}
	for range tiles {
		dir.varint(uint64(len(data)))
	}
	// Every tile shares the one stored copy.
	for range tiles {
		dir.varint(1)
	}
	root, meta := gz(dir.b), gz([]byte(metadata))
	header := make([]byte, 127)
	copy(header, "PMTiles")
	header[7] = 3
	put := func(at int, v uint64) { binary.LittleEndian.PutUint64(header[at:], v) }
	put(8, 127)
	put(16, uint64(len(root)))
	put(24, uint64(127+len(root)))
	put(32, uint64(len(meta)))
	put(40, uint64(127+len(root)+len(meta)))
	put(56, uint64(127+len(root)+len(meta)))
	put(64, uint64(len(data)))
	header[96], header[97], header[98], header[99], header[100], header[101] = 0, 2, 2, 1, 15, 15
	deg := func(at int, v float64) { binary.LittleEndian.PutUint32(header[at:], uint32(int32(math.Round(v*1e7)))) }
	deg(102, minLon)
	deg(106, minLat)
	deg(110, maxLon)
	deg(114, maxLat)
	out := append(header, root...)
	out = append(out, meta...)
	return append(out, data...)
}
