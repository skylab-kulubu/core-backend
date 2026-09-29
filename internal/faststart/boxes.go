package faststart

import (
	"context"
	"encoding/binary"
	"math"
)

// header is a box's header: its type, where it starts, its whole size and
// the header's own length (8, or 16 with a 64-bit size).
type header struct {
	typ   string
	off   int64
	size  int64
	head  int64
	large bool
}

// parseHeader reads the header of the box at the start of b, at off in the
// file, with room bytes to the end of what holds it. A size of 0 runs to
// that end, which only a top-level box may do (toEnd).
func parseHeader(b []byte, off, room int64, toEnd bool) (header, error) {
	if len(b) < 8 || room < 8 {
		return header{}, invalid("%d stray bytes at %d", min(int64(len(b)), room), off)
	}
	h := header{typ: string(b[4:8]), off: off, head: 8}
	size := int64(binary.BigEndian.Uint32(b))
	switch size {
	case 0:
		if !toEnd {
			return header{}, invalid("a %q box at %d runs to the end of its parent", h.typ, off)
		}
		size = room
	case 1:
		if len(b) < 16 || room < 16 {
			return header{}, invalid("a %q box at %d is cut short in its header", h.typ, off)
		}
		large := binary.BigEndian.Uint64(b[8:16])
		if large > math.MaxInt64 {
			return header{}, invalid("a %q box at %d declares %d bytes", h.typ, off, large)
		}
		size, h.head, h.large = int64(large), 16, true
	}
	if size < h.head {
		return header{}, invalid("a %q box at %d is %d bytes, shorter than its header", h.typ, off, size)
	}
	if size > room {
		return header{}, invalid("a %q box at %d is %d bytes, past the end of its parent", h.typ, off, size)
	}
	h.size = size
	return h, nil
}

// printable reports whether a top-level box type is four printable ASCII
// characters, as every top-level box of an MP4 is.
func printable(typ string) bool {
	for i := 0; i < len(typ); i++ {
		if typ[i] < 0x20 || typ[i] > 0x7e {
			return false
		}
	}
	return len(typ) == 4
}

// headerWindow is how much a top-level walk reads at a time: the headers of
// the small boxes that open a file come in one read.
const headerWindow = 16 << 10

// window reads a file by ranges, keeping the last range read.
type window struct {
	src  Source
	size int64
	off  int64
	buf  []byte
}

func (w *window) read(ctx context.Context, off, n int64) ([]byte, error) {
	if off >= w.off && off+n <= w.off+int64(len(w.buf)) {
		return w.buf[off-w.off : off-w.off+n], nil
	}
	buf, err := readRange(ctx, w.src, off, min(max(n, headerWindow), w.size-off))
	if err != nil {
		return nil, err
	}
	w.off, w.buf = off, buf
	return buf[:n], nil
}

// walkTop reads the headers of the file's top-level boxes, which must
// cover it exactly.
func walkTop(ctx context.Context, src Source, size int64, limits Limits) ([]header, error) {
	w := &window{src: src, size: size}
	var out []header
	for at := int64(0); at < size; {
		if len(out) == limits.MaxTopLevelBoxes {
			return nil, invalid("more than %d top-level boxes", limits.MaxTopLevelBoxes)
		}
		if size-at < 8 {
			return nil, invalid("%d stray bytes at the end of the file", size-at)
		}
		b, err := w.read(ctx, at, min(16, size-at))
		if err != nil {
			return nil, err
		}
		if !printable(string(b[4:8])) {
			return nil, invalid("the box at %d has no printable type", at)
		}
		h, err := parseHeader(b, at, size-at, true)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
		at += h.size
	}
	return out, nil
}

// node is a box of the moov as the rewrite keeps it: kept as it is (raw),
// a container it walks into (children, and any bytes after the last child),
// or a chunk offset table it rewrites.
type node struct {
	typ       string
	large     bool
	raw       []byte
	container bool
	children  []*node
	tail      []byte
	table     *offsetTable
}

// walked are the containers the rewrite walks into: each names the child
// that leads to the chunk offset tables.
var walked = map[string]string{"moov": "trak", "trak": "mdia", "mdia": "minf", "minf": "stbl"}

// moovParser reads the moov into nodes, counting boxes against the limit.
type moovParser struct {
	limits Limits
	boxes  int
	tables []*offsetTable
}

// container reads the container box b (whose header is h) and the boxes
// in it.
func (p *moovParser) container(b []byte, h header) (*node, error) {
	n := &node{typ: h.typ, large: h.large, container: true}
	payload := b[h.head:h.size]
	for at := 0; at < len(payload); {
		rest := payload[at:]
		if len(rest) < 8 {
			// QuickTime ends some containers with a zero terminator.
			for _, c := range rest {
				if c != 0 {
					return nil, invalid("%d stray bytes at the end of a %q box", len(rest), h.typ)
				}
			}
			n.tail = rest
			break
		}
		p.boxes++
		if p.boxes > p.limits.MaxMoovBoxes {
			return nil, invalid("more than %d boxes in the moov box", p.limits.MaxMoovBoxes)
		}
		off := h.off + h.head + int64(at)
		ch, err := parseHeader(rest, off, int64(len(rest)), false)
		if err != nil {
			return nil, err
		}
		child, err := p.child(rest[:ch.size], ch, h.typ)
		if err != nil {
			return nil, err
		}
		n.children = append(n.children, child)
		at += int(ch.size)
	}
	return n, nil
}

// child reads one box of the container named parent.
func (p *moovParser) child(b []byte, h header, parent string) (*node, error) {
	switch {
	case parent == "moov" && (h.typ == "cmov" || h.typ == "mvex"):
		return nil, invalid("a %s box in the moov box: a compressed or fragmented file", h.typ)
	case parent == "stbl" && h.typ == "saio":
		return nil, invalid("sample auxiliary information offsets (saio) at %d", h.off)
	case parent == "stbl" && (h.typ == "stco" || h.typ == "co64"):
		t, err := parseTable(b, h)
		if err != nil {
			return nil, err
		}
		p.tables = append(p.tables, t)
		return &node{typ: h.typ, large: h.large, table: t}, nil
	case walked[parent] == h.typ:
		return p.container(b, h)
	case (parent == "moov" || parent == "trak") && h.typ == "meta":
		if err := p.checkMeta(b, h); err != nil {
			return nil, err
		}
	case (parent == "moov" || parent == "trak") && h.typ == "udta":
		if err := p.checkUserData(b, h); err != nil {
			return nil, err
		}
	case parent == "minf" && h.typ == "dinf":
		if err := p.checkDataReferences(b, h); err != nil {
			return nil, err
		}
	}
	return &node{typ: h.typ, raw: b}, nil
}

// children reads the boxes that make up payload (at off in the file),
// counting them against the limit; zero bytes after the last one (a
// QuickTime terminator) are allowed.
func (p *moovParser) children(payload []byte, off int64, parent string) ([]header, error) {
	var out []header
	for at := 0; at < len(payload); {
		rest := payload[at:]
		if len(rest) < 8 {
			for _, c := range rest {
				if c != 0 {
					return nil, invalid("%d stray bytes at the end of a %q box", len(rest), parent)
				}
			}
			break
		}
		p.boxes++
		if p.boxes > p.limits.MaxMoovBoxes {
			return nil, invalid("more than %d boxes in the moov box", p.limits.MaxMoovBoxes)
		}
		h, err := parseHeader(rest, off+int64(at), int64(len(rest)), false)
		if err != nil {
			return nil, err
		}
		h.off = int64(at) // within payload
		out = append(out, h)
		at += int(h.size)
	}
	return out, nil
}

// checkMeta refuses a meta box that holds item locations (iloc): they
// may name absolute file offsets (HEIF-style items), which the rewrite
// does not move. ISO writes meta as a full box (version and flags first);
// QuickTime does not, and starts with its handler. Any iloc is refused,
// even one whose items would not move: simpler, and such files are rare.
func (p *moovParser) checkMeta(b []byte, h header) error {
	payload := b[h.head:h.size]
	from := 4
	if len(payload) >= 8 && string(payload[4:8]) == "hdlr" {
		from = 0
	}
	if len(payload) < from {
		return invalid("a meta box at %d is too short for its version and flags", h.off)
	}
	kids, err := p.children(payload[from:], h.off+h.head+int64(from), "meta")
	if err != nil {
		return err
	}
	for _, k := range kids {
		if k.typ == "iloc" {
			return invalid("item locations (iloc) in a meta box at %d, which the rewrite does not move", h.off)
		}
	}
	return nil
}

// checkUserData checks the meta boxes in a udta box.
func (p *moovParser) checkUserData(b []byte, h header) error {
	payload := b[h.head:h.size]
	kids, err := p.children(payload, h.off+h.head, "udta")
	if err != nil {
		return err
	}
	for _, k := range kids {
		if k.typ == "meta" {
			if err := p.checkMeta(payload[k.off:k.off+k.size], header{typ: k.typ, off: h.off + h.head + k.off, size: k.size, head: k.head, large: k.large}); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkDataReferences refuses a track whose samples may be in another
// file: a data reference (dinf/dref) that is not self-contained (flag 1
// clear). Its chunk offsets are that file's, and must not move.
func (p *moovParser) checkDataReferences(b []byte, h header) error {
	payload := b[h.head:h.size]
	kids, err := p.children(payload, h.off+h.head, "dinf")
	if err != nil {
		return err
	}
	for _, k := range kids {
		if k.typ != "dref" {
			continue
		}
		body := payload[k.off+k.head : k.off+k.size]
		if len(body) < 8 {
			return invalid("a dref box at %d is too short for its entry count", h.off+h.head+k.off)
		}
		entries, err := p.children(body[8:], h.off+h.head+k.off+k.head+8, "dref")
		if err != nil {
			return err
		}
		for _, e := range entries {
			entry := body[8+e.off+e.head : 8+e.off+e.size]
			if len(entry) < 4 || entry[3]&1 == 0 {
				return invalid("a data reference (%q) at %d that is not self-contained: samples in another file", e.typ, h.off+h.head+k.off)
			}
		}
	}
	return nil
}

// offsetTable is a track's chunk offsets: an stco (32-bit) or a co64
// (64-bit), as the source holds them.
type offsetTable struct {
	wide    bool
	flags   []byte
	count   int
	entries []byte
}

func parseTable(b []byte, h header) (*offsetTable, error) {
	body := b[h.head:]
	if len(body) < 8 {
		return nil, invalid("a %s box at %d is too short for its entry count", h.typ, h.off)
	}
	t := &offsetTable{wide: h.typ == "co64", flags: body[:4], entries: body[8:]}
	count := int64(binary.BigEndian.Uint32(body[4:8]))
	if int64(len(t.entries)) != count*t.width() {
		return nil, invalid("a %s box at %d of %d entries is %d bytes", h.typ, h.off, count, h.size)
	}
	t.count = int(count)
	return t, nil
}

func (t *offsetTable) typ() string {
	if t.wide {
		return "co64"
	}
	return "stco"
}

func (t *offsetTable) width() int64 {
	if t.wide {
		return 8
	}
	return 4
}

// entry is the source's chunk offset i.
func (t *offsetTable) entry(i int) int64 {
	if t.wide {
		return int64(binary.BigEndian.Uint64(t.entries[i*8:]))
	}
	return int64(binary.BigEndian.Uint32(t.entries[i*4:]))
}

func headerLen(large bool) int64 {
	if large {
		return 16
	}
	return 8
}

// size is the node's size once rewritten; upgrade makes every stco a co64.
func (n *node) size(upgrade bool) int64 {
	switch {
	case n.table != nil:
		width := int64(4)
		if n.table.wide || upgrade {
			width = 8
		}
		return headerLen(n.large) + 8 + int64(n.table.count)*width
	case n.container:
		size := headerLen(n.large) + int64(len(n.tail))
		for _, c := range n.children {
			size += c.size(upgrade)
		}
		return size
	}
	return int64(len(n.raw))
}

// write appends the rewritten node to out, every chunk offset moved (m).
// The offsets were checked (mover.check) before.
func (n *node) write(out []byte, upgrade bool, m mover) []byte {
	switch {
	case n.table != nil:
		t := n.table
		wide := t.wide || upgrade
		typ := "stco"
		if wide {
			typ = "co64"
		}
		out = appendHeader(out, typ, n.size(upgrade), n.large)
		out = append(out, t.flags...)
		out = binary.BigEndian.AppendUint32(out, uint32(t.count))
		for i := 0; i < t.count; i++ {
			moved, _ := m.move(t.entry(i))
			if wide {
				out = binary.BigEndian.AppendUint64(out, uint64(moved))
			} else {
				out = binary.BigEndian.AppendUint32(out, uint32(moved))
			}
		}
		return out
	case n.container:
		out = appendHeader(out, n.typ, n.size(upgrade), n.large)
		for _, c := range n.children {
			out = c.write(out, upgrade, m)
		}
		return append(out, n.tail...)
	}
	return append(out, n.raw...)
}

// appendHeader writes a box header in the form the box had: a 64-bit size
// when it had one (or needs one), a 32-bit size otherwise.
func appendHeader(out []byte, typ string, size int64, large bool) []byte {
	if large || size > math.MaxUint32 {
		out = binary.BigEndian.AppendUint32(out, 1)
		out = append(out, typ...)
		return binary.BigEndian.AppendUint64(out, uint64(size))
	}
	out = binary.BigEndian.AppendUint32(out, uint32(size))
	return append(out, typ...)
}
