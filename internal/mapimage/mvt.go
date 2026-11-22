package mapimage

import (
	"encoding/binary"
	"errors"
	"math"
)

// A vector tile (Mapbox Vector Tile 2.1): its layers, each with features of
// one geometry and their properties. Only what the map draws is read: the
// names and kinds of features, as text, and their geometry.

var errBadTile = errors.New("mapimage: a vector tile that does not parse")

// Geometry types.
const (
	geomPoint   = 1
	geomLine    = 2
	geomPolygon = 3
)

type point struct{ x, y float64 }

type feature struct {
	kind  int
	props map[string]string
	// parts are a line's lines or a polygon's rings, in tile coordinates.
	parts [][]point
}

type layer struct {
	name     string
	extent   float64
	features []feature
}

// wire reads protocol buffer fields.
type wire struct {
	b []byte
}

func (w *wire) varint() (uint64, bool) {
	v, n := binary.Uvarint(w.b)
	if n <= 0 {
		return 0, false
	}
	w.b = w.b[n:]
	return v, true
}

// field reads the next field's number, its wire type, and its bytes (a
// length-delimited field) or its value (a varint or fixed one).
func (w *wire) field() (num uint64, typ uint64, data []byte, value uint64, ok bool) {
	key, ok := w.varint()
	if !ok {
		return 0, 0, nil, 0, false
	}
	num, typ = key>>3, key&7
	switch typ {
	case 0:
		value, ok = w.varint()
	case 1:
		if len(w.b) < 8 {
			return 0, 0, nil, 0, false
		}
		value, w.b = binary.LittleEndian.Uint64(w.b), w.b[8:]
	case 2:
		var n uint64
		if n, ok = w.varint(); !ok || n > uint64(len(w.b)) {
			return 0, 0, nil, 0, false
		}
		data, w.b = w.b[:n], w.b[n:]
	case 5:
		if len(w.b) < 4 {
			return 0, 0, nil, 0, false
		}
		value, w.b = uint64(binary.LittleEndian.Uint32(w.b)), w.b[4:]
	default:
		return 0, 0, nil, 0, false
	}
	return num, typ, data, value, ok
}

func packed(b []byte) ([]uint32, bool) {
	w := wire{b: b}
	out := make([]uint32, 0, len(b))
	for len(w.b) > 0 {
		v, ok := w.varint()
		if !ok || v > math.MaxUint32 {
			return nil, false
		}
		out = append(out, uint32(v))
	}
	return out, true
}

// decodeTile reads the tile's layers named in want (all with nil).
func decodeTile(b []byte, want map[string]bool) ([]layer, error) {
	w := wire{b: b}
	var out []layer
	for len(w.b) > 0 {
		num, typ, data, _, ok := w.field()
		if !ok {
			return nil, errBadTile
		}
		if num != 3 || typ != 2 {
			continue
		}
		l, err := decodeLayer(data, want)
		if err != nil {
			return nil, err
		}
		if l != nil {
			out = append(out, *l)
		}
	}
	return out, nil
}

func decodeLayer(b []byte, want map[string]bool) (*layer, error) {
	w := wire{b: b}
	l := layer{extent: 4096}
	var keys []string
	var values []string
	var raw [][]byte
	for len(w.b) > 0 {
		num, typ, data, value, ok := w.field()
		if !ok {
			return nil, errBadTile
		}
		switch {
		case num == 1 && typ == 2:
			l.name = string(data)
		case num == 2 && typ == 2:
			raw = append(raw, data)
		case num == 3 && typ == 2:
			keys = append(keys, string(data))
		case num == 4 && typ == 2:
			values = append(values, decodeValue(data))
		case num == 5 && typ == 0:
			if value > 0 {
				l.extent = float64(value)
			}
		}
	}
	if want != nil && !want[l.name] {
		return nil, nil
	}
	for _, f := range raw {
		feat, ok := decodeFeature(f, keys, values)
		if !ok {
			return nil, errBadTile
		}
		l.features = append(l.features, feat)
	}
	return &l, nil
}

// decodeValue is a property's value as text: what the map reads (names,
// kinds, flags) is text or a boolean.
func decodeValue(b []byte) string {
	w := wire{b: b}
	for len(w.b) > 0 {
		num, typ, data, value, ok := w.field()
		if !ok {
			return ""
		}
		switch {
		case num == 1 && typ == 2:
			return string(data)
		case num == 7 && typ == 0:
			if value != 0 {
				return "true"
			}
			return "false"
		}
	}
	return ""
}

func decodeFeature(b []byte, keys, values []string) (feature, bool) {
	w := wire{b: b}
	f := feature{props: map[string]string{}}
	var geometry []uint32
	for len(w.b) > 0 {
		num, typ, data, value, ok := w.field()
		if !ok {
			return feature{}, false
		}
		switch {
		case num == 2 && typ == 2:
			tags, ok := packed(data)
			if !ok || len(tags)%2 != 0 {
				return feature{}, false
			}
			for i := 0; i < len(tags); i += 2 {
				if int(tags[i]) < len(keys) && int(tags[i+1]) < len(values) {
					f.props[keys[tags[i]]] = values[tags[i+1]]
				}
			}
		case num == 3 && typ == 0:
			f.kind = int(value)
		case num == 4 && typ == 2:
			if geometry, ok = packed(data); !ok {
				return feature{}, false
			}
		}
	}
	f.parts = decodeGeometry(geometry)
	return f, true
}

func zigzag(v uint32) float64 {
	return float64(int32(v>>1) ^ -int32(v&1))
}

// decodeGeometry follows the commands: MoveTo starts a part, LineTo extends
// it, ClosePath closes a ring.
func decodeGeometry(cmds []uint32) [][]point {
	var parts [][]point
	var cur []point
	var x, y float64
	for i := 0; i < len(cmds); {
		id, count := cmds[i]&7, int(cmds[i]>>3)
		i++
		switch id {
		case 1, 2:
			if i+2*count > len(cmds) {
				return parts
			}
			for c := 0; c < count; c++ {
				x += zigzag(cmds[i])
				y += zigzag(cmds[i+1])
				i += 2
				if id == 1 {
					if len(cur) > 0 {
						parts = append(parts, cur)
					}
					cur = []point{{x, y}}
				} else {
					cur = append(cur, point{x, y})
				}
			}
		case 7:
			if len(cur) > 0 {
				cur = append(cur, cur[0])
			}
		default:
			return parts
		}
	}
	if len(cur) > 0 {
		parts = append(parts, cur)
	}
	return parts
}
