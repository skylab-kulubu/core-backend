// Package mp4test builds minimal ISO-BMFF (MP4) files box by box for tests,
// and reads them back with a parser of its own: a test of the faststart
// rewrite checks its output with code it does not share.
package mp4test

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"testing"
)

// The fixtures are minimal ISO-BMFF files built box by box: an ftyp, an
// mdat of chunks that each name themselves, and a moov with one trak per
// track whose stbl lists the chunks' offsets (stco or co64).

// U32 and U64 are v big-endian, as a box holds numbers.
func U32(v uint32) []byte { return binary.BigEndian.AppendUint32(nil, v) }
func U64(v uint64) []byte { return binary.BigEndian.AppendUint64(nil, v) }

// Box is a box with a 32-bit size.
func Box(typ string, payload ...[]byte) []byte {
	body := bytes.Join(payload, nil)
	return append(append(U32(uint32(8+len(body))), typ...), body...)
}

// LargeBox is a box with a 64-bit size (size field 1).
func LargeBox(typ string, payload ...[]byte) []byte {
	body := bytes.Join(payload, nil)
	out := append(U32(1), typ...)
	out = append(out, U64(uint64(16+len(body)))...)
	return append(out, body...)
}

// FullBox is a box with a version and flags.
func FullBox(typ string, version byte, flags uint32, payload ...[]byte) []byte {
	return Box(typ, append([]byte{version, byte(flags >> 16), byte(flags >> 8), byte(flags)}, bytes.Join(payload, nil)...))
}

// Stco is a chunk offset table of 32-bit offsets.
func Stco(offsets ...uint32) []byte {
	payload := U32(uint32(len(offsets)))
	for _, o := range offsets {
		payload = append(payload, U32(o)...)
	}
	return FullBox("stco", 0, 0, payload)
}

// Co64 is a chunk offset table of 64-bit offsets.
func Co64(offsets ...uint64) []byte {
	payload := U32(uint32(len(offsets)))
	for _, o := range offsets {
		payload = append(payload, U64(o)...)
	}
	return FullBox("co64", 0, 0, payload)
}

// Ftyp is an isom ftyp box.
func Ftyp() []byte {
	return Box("ftyp", []byte("isom"), U32(0x200), []byte("isomiso2mp41"))
}

// Trak is a track whose sample table holds the chunk offset box given.
func Trak(id uint32, chunkOffsets []byte) []byte {
	return Box("trak",
		FullBox("tkhd", 0, 3, make([]byte, 8), U32(id), make([]byte, 68)),
		Box("mdia",
			FullBox("mdhd", 0, 0, make([]byte, 8), U32(1000), U32(0), make([]byte, 4)),
			FullBox("hdlr", 0, 0, U32(0), []byte("vide"), make([]byte, 12), []byte("VideoHandler\x00")),
			Box("minf",
				FullBox("vmhd", 0, 1, make([]byte, 8)),
				Box("dinf", FullBox("dref", 0, 0, U32(1), FullBox("url ", 0, 1))),
				Box("stbl",
					FullBox("stsd", 0, 0, U32(0)),
					FullBox("stts", 0, 0, U32(0)),
					FullBox("stsc", 0, 0, U32(0)),
					FullBox("stsz", 0, 0, U32(0), U32(0)),
					chunkOffsets,
				),
			),
		),
	)
}

// Moov is a moov box: an mvhd and the tracks.
func Moov(traks ...[]byte) []byte {
	return Box("moov", append([][]byte{FullBox("mvhd", 0, 0, make([]byte, 96))}, traks...)...)
}

// Chunk is a chunk's bytes: its name, then filler.
func Chunk(name string, n int) []byte {
	out := []byte(fmt.Sprintf("<%s>", name))
	for len(out) < n {
		out = append(out, byte(len(out)))
	}
	return out[:n]
}

// Movie is an MP4 with its moov after its mdat, as a camera writes it: an
// ftyp, an mdat holding the chunks, and a moov with one trak per track,
// each listing its chunks' offsets in an stco. tracks[i] are the chunk
// indexes of track i.
type Movie struct {
	Chunks [][]byte
	Tracks [][]int
	Wide   bool // co64 instead of stco
	// before are boxes between the ftyp and the mdat; after, boxes after
	// the moov.
	Before, After [][]byte
	// largeMdat writes the mdat with a 64-bit size.
	LargeMdat bool
}

// Build lays the movie out and returns the file and each chunk's offset.
func (m Movie) Build() ([]byte, []int64) {
	file := append([]byte{}, Ftyp()...)
	for _, b := range m.Before {
		file = append(file, b...)
	}
	header := 8
	if m.LargeMdat {
		header = 16
	}
	offsets := make([]int64, len(m.Chunks))
	at := int64(len(file) + header)
	var data []byte
	for i, c := range m.Chunks {
		offsets[i] = at
		at += int64(len(c))
		data = append(data, c...)
	}
	if m.LargeMdat {
		file = append(file, LargeBox("mdat", data)...)
	} else {
		file = append(file, Box("mdat", data)...)
	}
	file = append(file, m.Moov(offsets)...)
	for _, b := range m.After {
		file = append(file, b...)
	}
	return file, offsets
}

// Moov is the movie's moov box for its chunks at offsets.
func (m Movie) Moov(offsets []int64) []byte {
	var traks [][]byte
	for i, chunks := range m.Tracks {
		var table []byte
		if m.Wide {
			var list []uint64
			for _, c := range chunks {
				list = append(list, uint64(offsets[c]))
			}
			table = Co64(list...)
		} else {
			var list []uint32
			for _, c := range chunks {
				list = append(list, uint32(offsets[c]))
			}
			table = Stco(list...)
		}
		traks = append(traks, Trak(uint32(i+1), table))
	}
	return Moov(traks...)
}

// TwoTracks is a movie of a video and an audio track, their chunks
// interleaved.
func TwoTracks() Movie {
	return Movie{
		Chunks: [][]byte{Chunk("v0", 40), Chunk("a0", 24), Chunk("v1", 40), Chunk("a1", 24), Chunk("v2", 40)},
		Tracks: [][]int{{0, 2, 4}, {1, 3}},
	}
}

// Memory is a file in memory, read by ranges.
type Memory []byte

func (m Memory) OpenRange(_ context.Context, off, n int64) (io.ReadCloser, error) {
	if off < 0 || n <= 0 || off+n > int64(len(m)) {
		return nil, fmt.Errorf("range %d+%d outside %d bytes", off, n, len(m))
	}
	return io.NopCloser(bytes.NewReader(m[off : off+n])), nil
}

// Parsed is a box as the tests read it back, with a parser of their own.
type Parsed struct {
	Type        string
	Off, Size   int64
	Header      int64
	PayloadFrom int64
}

// Boxes reads the boxes of file from from to to, which they must cover.
func Boxes(t testing.TB, file []byte, from, to int64) []Parsed {
	t.Helper()
	var out []Parsed
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
		out = append(out, Parsed{Type: string(file[at+4 : at+8]), Off: at, Size: size, Header: header, PayloadFrom: at + header})
		at += size
	}
	return out
}

// Types are the boxes' types, in order.
func Types(bs []Parsed) []string {
	var out []string
	for _, b := range bs {
		out = append(out, b.Type)
	}
	return out
}

func find(t testing.TB, file []byte, within Parsed, typ string) []Parsed {
	t.Helper()
	var out []Parsed
	for _, b := range Boxes(t, file, within.PayloadFrom, within.Off+within.Size) {
		if b.Type == typ {
			out = append(out, b)
		}
	}
	return out
}

// ChunkOffsets reads every track's chunk offsets from the file's moov, and
// whether each track's table is a co64.
func ChunkOffsets(t testing.TB, file []byte) ([][]int64, []bool) {
	t.Helper()
	var movies []Parsed
	for _, b := range Boxes(t, file, 0, int64(len(file))) {
		if b.Type == "moov" {
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
		count := int64(binary.BigEndian.Uint32(file[table.PayloadFrom+4:]))
		entries := table.PayloadFrom + 8
		width := int64(4)
		if table.Type == "co64" {
			width = 8
		}
		if table.Off+table.Size != entries+count*width {
			t.Fatalf("%s of %d entries is %d bytes", table.Type, count, table.Size)
		}
		for i := int64(0); i < count; i++ {
			if width == 4 {
				offsets = append(offsets, int64(binary.BigEndian.Uint32(file[entries+i*4:])))
			} else {
				offsets = append(offsets, int64(binary.BigEndian.Uint64(file[entries+i*8:])))
			}
		}
		tracks = append(tracks, offsets)
		wide = append(wide, table.Type == "co64")
	}
	return tracks, wide
}

// AssertSamePlayback checks the rewritten file holds the moov before its
// media data, and every chunk's bytes at the offset its table now names.
func AssertSamePlayback(t testing.TB, m Movie, rewritten []byte) {
	t.Helper()
	top := Types(Boxes(t, rewritten, 0, int64(len(rewritten))))
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
	tracks, _ := ChunkOffsets(t, rewritten)
	if len(tracks) != len(m.Tracks) {
		t.Fatalf("%d tracks, want %d", len(tracks), len(m.Tracks))
	}
	for i, chunks := range m.Tracks {
		if len(tracks[i]) != len(chunks) {
			t.Fatalf("track %d lists %d chunks, want %d", i, len(tracks[i]), len(chunks))
		}
		for j, c := range chunks {
			want := m.Chunks[c]
			at := tracks[i][j]
			if at+int64(len(want)) > int64(len(rewritten)) || !bytes.Equal(rewritten[at:at+int64(len(want))], want) {
				t.Fatalf("track %d chunk %d: the bytes at %d are not the chunk", i, j, at)
			}
		}
	}
}

// Sparse is a large file that is zeros but for the pieces placed in it,
// read by ranges: a 4 GiB mdat costs nothing.
type Sparse struct {
	Size   int64
	pieces []piece
}

type piece struct {
	at   int64
	data []byte
}

// Place puts data at at.
func (s *Sparse) Place(at int64, data []byte) {
	s.pieces = append(s.pieces, piece{at: at, data: data})
}

func (s *Sparse) OpenRange(_ context.Context, off, n int64) (io.ReadCloser, error) {
	if off < 0 || n <= 0 || off+n > s.Size {
		return nil, fmt.Errorf("range %d+%d outside %d bytes", off, n, s.Size)
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
