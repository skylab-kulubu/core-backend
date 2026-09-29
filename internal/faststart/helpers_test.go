package faststart_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/faststart"
)

// The fixtures are minimal ISO-BMFF files built box by box: an ftyp, an
// mdat of chunks that each name themselves, and a moov with one trak per
// track whose stbl lists the chunks' offsets (stco or co64).

func be32(v uint32) []byte { return binary.BigEndian.AppendUint32(nil, v) }
func be64(v uint64) []byte { return binary.BigEndian.AppendUint64(nil, v) }

// box is a box with a 32-bit size.
func box(typ string, payload ...[]byte) []byte {
	body := bytes.Join(payload, nil)
	return append(append(be32(uint32(8+len(body))), typ...), body...)
}

// largeBox is a box with a 64-bit size (size field 1).
func largeBox(typ string, payload ...[]byte) []byte {
	body := bytes.Join(payload, nil)
	out := append(be32(1), typ...)
	out = append(out, be64(uint64(16+len(body)))...)
	return append(out, body...)
}

// fullBox is a box with a version and flags.
func fullBox(typ string, version byte, flags uint32, payload ...[]byte) []byte {
	return box(typ, append([]byte{version, byte(flags >> 16), byte(flags >> 8), byte(flags)}, bytes.Join(payload, nil)...))
}

func stco(offsets ...uint32) []byte {
	payload := be32(uint32(len(offsets)))
	for _, o := range offsets {
		payload = append(payload, be32(o)...)
	}
	return fullBox("stco", 0, 0, payload)
}

func co64(offsets ...uint64) []byte {
	payload := be32(uint32(len(offsets)))
	for _, o := range offsets {
		payload = append(payload, be64(o)...)
	}
	return fullBox("co64", 0, 0, payload)
}

func ftyp() []byte {
	return box("ftyp", []byte("isom"), be32(0x200), []byte("isomiso2mp41"))
}

// trak is a track whose sample table holds the chunk offset box given.
func trak(id uint32, chunkOffsets []byte) []byte {
	return box("trak",
		fullBox("tkhd", 0, 3, make([]byte, 8), be32(id), make([]byte, 68)),
		box("mdia",
			fullBox("mdhd", 0, 0, make([]byte, 8), be32(1000), be32(0), make([]byte, 4)),
			fullBox("hdlr", 0, 0, be32(0), []byte("vide"), make([]byte, 12), []byte("VideoHandler\x00")),
			box("minf",
				fullBox("vmhd", 0, 1, make([]byte, 8)),
				box("dinf", fullBox("dref", 0, 0, be32(1), fullBox("url ", 0, 1))),
				box("stbl",
					fullBox("stsd", 0, 0, be32(0)),
					fullBox("stts", 0, 0, be32(0)),
					fullBox("stsc", 0, 0, be32(0)),
					fullBox("stsz", 0, 0, be32(0), be32(0)),
					chunkOffsets,
				),
			),
		),
	)
}

func moov(traks ...[]byte) []byte {
	return box("moov", append([][]byte{fullBox("mvhd", 0, 0, make([]byte, 96))}, traks...)...)
}

// chunk is a chunk's bytes: its name, then filler.
func chunk(name string, n int) []byte {
	out := []byte(fmt.Sprintf("<%s>", name))
	for len(out) < n {
		out = append(out, byte(len(out)))
	}
	return out[:n]
}

// movie is an MP4 with its moov after its mdat, as a camera writes it: an
// ftyp, an mdat holding the chunks, and a moov with one trak per track,
// each listing its chunks' offsets in an stco. tracks[i] are the chunk
// indexes of track i.
type movie struct {
	chunks [][]byte
	tracks [][]int
	wide   bool // co64 instead of stco
	// before are boxes between the ftyp and the mdat; after, boxes after
	// the moov.
	before, after [][]byte
	// largeMdat writes the mdat with a 64-bit size.
	largeMdat bool
}

// build lays the movie out and returns the file and each chunk's offset.
func (m movie) build() ([]byte, []int64) {
	file := append([]byte{}, ftyp()...)
	for _, b := range m.before {
		file = append(file, b...)
	}
	header := 8
	if m.largeMdat {
		header = 16
	}
	offsets := make([]int64, len(m.chunks))
	at := int64(len(file) + header)
	var data []byte
	for i, c := range m.chunks {
		offsets[i] = at
		at += int64(len(c))
		data = append(data, c...)
	}
	if m.largeMdat {
		file = append(file, largeBox("mdat", data)...)
	} else {
		file = append(file, box("mdat", data)...)
	}
	file = append(file, m.moov(offsets)...)
	for _, b := range m.after {
		file = append(file, b...)
	}
	return file, offsets
}

func (m movie) moov(offsets []int64) []byte {
	var traks [][]byte
	for i, chunks := range m.tracks {
		var table []byte
		if m.wide {
			var list []uint64
			for _, c := range chunks {
				list = append(list, uint64(offsets[c]))
			}
			table = co64(list...)
		} else {
			var list []uint32
			for _, c := range chunks {
				list = append(list, uint32(offsets[c]))
			}
			table = stco(list...)
		}
		traks = append(traks, trak(uint32(i+1), table))
	}
	return moov(traks...)
}

func twoTracks() movie {
	return movie{
		chunks: [][]byte{chunk("v0", 40), chunk("a0", 24), chunk("v1", 40), chunk("a1", 24), chunk("v2", 40)},
		tracks: [][]int{{0, 2, 4}, {1, 3}},
	}
}

// memory is a file in memory, read by ranges.
type memory []byte

func (m memory) OpenRange(_ context.Context, off, n int64) (io.ReadCloser, error) {
	if off < 0 || n <= 0 || off+n > int64(len(m)) {
		return nil, fmt.Errorf("range %d+%d outside %d bytes", off, n, len(m))
	}
	return io.NopCloser(bytes.NewReader(m[off : off+n])), nil
}

// rewrite plans the rewrite of file and returns the rewritten file whole.
func rewrite(t *testing.T, file []byte) ([]byte, faststart.Layout) {
	t.Helper()
	layout, err := faststart.Plan(context.Background(), memory(file), int64(len(file)), faststart.DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	out, err := layout.Read(context.Background(), memory(file), 0, layout.Size)
	if err != nil {
		t.Fatal(err)
	}
	return out, layout
}

// A box as the tests read it back, with a parser of their own.
type parsed struct {
	typ         string
	off, size   int64
	header      int64
	payloadFrom int64
}

func boxes(t *testing.T, file []byte, from, to int64) []parsed {
	t.Helper()
	var out []parsed
	for at := from; at < to; {
		if to-at < 8 {
			t.Fatalf("%d stray bytes at %d", to-at, at)
		}
		size := int64(binary.BigEndian.Uint32(file[at:]))
		header := int64(8)
		if size == 1 {
			size = int64(binary.BigEndian.Uint64(file[at+8:]))
			header = 16
		}
		if size < header || at+size > to {
			t.Fatalf("box %q at %d has size %d", file[at+4:at+8], at, size)
		}
		out = append(out, parsed{typ: string(file[at+4 : at+8]), off: at, size: size, header: header, payloadFrom: at + header})
		at += size
	}
	return out
}

func types(bs []parsed) []string {
	var out []string
	for _, b := range bs {
		out = append(out, b.typ)
	}
	return out
}

func find(t *testing.T, file []byte, within parsed, typ string) []parsed {
	t.Helper()
	var out []parsed
	for _, b := range boxes(t, file, within.payloadFrom, within.off+within.size) {
		if b.typ == typ {
			out = append(out, b)
		}
	}
	return out
}

// chunkOffsets reads every track's chunk offsets from the file's moov, and
// whether each track's table is a co64.
func chunkOffsets(t *testing.T, file []byte) ([][]int64, []bool) {
	t.Helper()
	var movies []parsed
	for _, b := range boxes(t, file, 0, int64(len(file))) {
		if b.typ == "moov" {
			movies = append(movies, b)
		}
	}
	if len(movies) != 1 {
		t.Fatalf("%d moov boxes", len(movies))
	}
	var tracks [][]int64
	var wide []bool
	for _, tr := range find(t, file, movies[0], "trak") {
		mdia := find(t, file, tr, "mdia")[0]
		minf := find(t, file, mdia, "minf")[0]
		stbl := find(t, file, minf, "stbl")[0]
		var offsets []int64
		tables := append(find(t, file, stbl, "stco"), find(t, file, stbl, "co64")...)
		if len(tables) != 1 {
			t.Fatalf("%d chunk offset tables in a trak", len(tables))
		}
		table := tables[0]
		count := int64(binary.BigEndian.Uint32(file[table.payloadFrom+4:]))
		entries := table.payloadFrom + 8
		width := int64(4)
		if table.typ == "co64" {
			width = 8
		}
		if table.off+table.size != entries+count*width {
			t.Fatalf("%s of %d entries is %d bytes", table.typ, count, table.size)
		}
		for i := int64(0); i < count; i++ {
			if width == 4 {
				offsets = append(offsets, int64(binary.BigEndian.Uint32(file[entries+i*4:])))
			} else {
				offsets = append(offsets, int64(binary.BigEndian.Uint64(file[entries+i*8:])))
			}
		}
		tracks = append(tracks, offsets)
		wide = append(wide, table.typ == "co64")
	}
	return tracks, wide
}

// assertSamePlayback checks the rewritten file holds the moov before its
// media data, and every chunk's bytes at the offset its table now names.
func assertSamePlayback(t *testing.T, m movie, rewritten []byte) {
	t.Helper()
	top := types(boxes(t, rewritten, 0, int64(len(rewritten))))
	moovAt, mdatAt := -1, -1
	for i, typ := range top {
		if typ == "moov" && moovAt < 0 {
			moovAt = i
		}
		if typ == "mdat" && mdatAt < 0 {
			mdatAt = i
		}
	}
	if top[0] != "ftyp" || moovAt < 0 || mdatAt < 0 || moovAt > mdatAt {
		t.Fatalf("top-level boxes %v: want the ftyp first and the moov before the mdat", top)
	}
	tracks, _ := chunkOffsets(t, rewritten)
	if len(tracks) != len(m.tracks) {
		t.Fatalf("%d tracks, want %d", len(tracks), len(m.tracks))
	}
	for i, chunks := range m.tracks {
		if len(tracks[i]) != len(chunks) {
			t.Fatalf("track %d lists %d chunks, want %d", i, len(tracks[i]), len(chunks))
		}
		for j, c := range chunks {
			want := m.chunks[c]
			at := tracks[i][j]
			if at+int64(len(want)) > int64(len(rewritten)) || !bytes.Equal(rewritten[at:at+int64(len(want))], want) {
				t.Fatalf("track %d chunk %d: the bytes at %d are not the chunk", i, j, at)
			}
		}
	}
}

// sparse is a large file that is zeros but for the pieces placed in it,
// read by ranges: a 4 GiB mdat costs nothing.
type sparse struct {
	size   int64
	pieces []piece
}

type piece struct {
	at   int64
	data []byte
}

func (s *sparse) place(at int64, data []byte) {
	s.pieces = append(s.pieces, piece{at: at, data: data})
}

func (s *sparse) OpenRange(_ context.Context, off, n int64) (io.ReadCloser, error) {
	if off < 0 || n <= 0 || off+n > s.size {
		return nil, fmt.Errorf("range %d+%d outside %d bytes", off, n, s.size)
	}
	out := make([]byte, n)
	for _, p := range s.pieces {
		lo, hi := max(off, p.at), min(off+n, p.at+int64(len(p.data)))
		if lo < hi {
			copy(out[lo-off:hi-off], p.data[lo-p.at:hi-p.at])
		}
	}
	return io.NopCloser(bytes.NewReader(out)), nil
}
