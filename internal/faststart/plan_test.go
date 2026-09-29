package faststart_test

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/faststart"
)

// A camera's MP4 has its moov after the media data. The rewrite puts the
// moov in front of it, and shifts every chunk offset of every track by the
// moov's size: each chunk's bytes are where its track's table now says.
func TestPlanMovesTheMoovInFrontOfTheMediaData(t *testing.T) {
	t.Parallel()
	m := twoTracks()
	file, _ := m.build()
	rewritten, layout := rewrite(t, file)
	if layout.Size != int64(len(file)) || len(rewritten) != len(file) {
		t.Fatalf("rewritten %d bytes (layout %d), want %d: an stco that fits keeps its size", len(rewritten), layout.Size, len(file))
	}
	if got := types(boxes(t, rewritten, 0, int64(len(rewritten)))); len(got) != 3 || got[0] != "ftyp" || got[1] != "moov" || got[2] != "mdat" {
		t.Fatalf("top-level boxes %v, want [ftyp moov mdat]", got)
	}
	assertSamePlayback(t, m, rewritten)
	_, wide := chunkOffsets(t, rewritten)
	for i, w := range wide {
		if w {
			t.Fatalf("track %d became a co64 without need", i)
		}
	}
}

// A file whose moov already comes before its media data, or that has no
// media data at all, needs nothing.
func TestPlanLeavesAFaststartFileAlone(t *testing.T) {
	t.Parallel()
	m := twoTracks()
	file, offsets := m.build()
	out, _ := rewrite(t, file)
	for name, f := range map[string][]byte{
		"moov first":    out,
		"no media data": append(ftyp(), m.moov(offsets)...),
	} {
		_, err := faststart.Plan(context.Background(), memory(f), int64(len(f)), faststart.DefaultLimits)
		if !errors.Is(err, faststart.ErrAlreadyFaststart) {
			t.Errorf("%s: %v, want ErrAlreadyFaststart", name, err)
		}
	}
}

// 64-bit chunk offsets (co64) and 64-bit box sizes move the same way.
func TestPlanMovesCo64AndLargeBoxes(t *testing.T) {
	t.Parallel()
	m := twoTracks()
	m.wide = true
	m.largeMdat = true
	file, _ := m.build()
	rewritten, _ := rewrite(t, file)
	assertSamePlayback(t, m, rewritten)
	if _, wide := chunkOffsets(t, rewritten); !wide[0] || !wide[1] {
		t.Fatalf("co64 tables became %v", wide)
	}
	mdat := boxes(t, rewritten, 0, int64(len(rewritten)))[2]
	if mdat.typ != "mdat" || mdat.header != 16 {
		t.Fatalf("the mdat is %q with a %d-byte header, want its 64-bit size kept", mdat.typ, mdat.header)
	}
}

// Boxes between the ftyp and the media data stay in front of the moov;
// boxes after the old moov stay at the end, and a chunk in an mdat after
// it moves with it.
func TestPlanKeepsTheOtherBoxesInPlace(t *testing.T) {
	t.Parallel()
	m := twoTracks()
	m.before = [][]byte{box("free", make([]byte, 20)), box("uuid", make([]byte, 16), []byte("xmp"))}
	file, offsets := m.build()
	late := chunk("v3", 32)
	file = append(file, box("free", make([]byte, 4))...)
	lateAt := int64(len(file) + 8)
	file = append(file, box("mdat", late)...)
	// Point track 0's last chunk at the mdat after the moov: the moov is
	// rebuilt with that offset, at the same size.
	m.chunks = append(m.chunks, late)
	m.tracks[0] = append(m.tracks[0][:2], len(m.chunks)-1)
	offsets = append(offsets, lateAt)
	head := bytes.Index(file, []byte("moov")) - 4
	moovBox := m.moov(offsets)
	file = append(append(append([]byte{}, file[:head]...), moovBox...), file[head+len(moovBox):]...)

	rewritten, _ := rewrite(t, file)
	got := types(boxes(t, rewritten, 0, int64(len(rewritten))))
	want := []string{"ftyp", "free", "uuid", "moov", "mdat", "free", "mdat"}
	if !slices.Equal(got, want) {
		t.Fatalf("top-level boxes %v, want %v", got, want)
	}
	assertSamePlayback(t, m, rewritten)
}

// An stco whose offsets would pass 4 GiB once moved becomes a co64, as
// qt-faststart does: every stco of the moov at once, the moov growing by
// four bytes an entry and the shift with it. The file is a sparse one of a
// little over 4 GiB, read only where the rewrite looks.
func TestPlanUpgradesAnStcoThatWouldOverflow(t *testing.T) {
	t.Parallel()
	high := []int64{0xFFFF_FF80, 0xFFFF_FFB0}
	low := []int64{100, 200}
	chunks := map[int64][]byte{high[0]: chunk("v0", 40), high[1]: chunk("v1", 40), low[0]: chunk("a0", 24), low[1]: chunk("a1", 24)}
	f := &sparse{}
	f.place(0, ftyp())
	mdatAt := int64(len(ftyp()))
	mdatEnd := int64(0xFFFF_FFE0)
	f.place(mdatAt, append(be32(1), append([]byte("mdat"), be64(uint64(mdatEnd-mdatAt))...)...))
	for at, c := range chunks {
		f.place(at, c)
	}
	moovBox := moov(
		trak(1, stco(uint32(high[0]), uint32(high[1]))),
		trak(2, stco(uint32(low[0]), uint32(low[1]))),
	)
	f.place(mdatEnd, moovBox)
	f.size = mdatEnd + int64(len(moovBox))

	ctx := context.Background()
	layout, err := faststart.Plan(ctx, f, f.size, faststart.DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	grown := int64(len(moovBox)) + 4*4
	if layout.Size != f.size+16 || layout.MoovAt != mdatAt {
		t.Fatalf("rewritten %d bytes with the moov at %d, want %d and %d", layout.Size, layout.MoovAt, f.size+16, mdatAt)
	}
	head, err := layout.Read(ctx, f, 0, layout.MoovAt+grown)
	if err != nil {
		t.Fatal(err)
	}
	tracks, wide := chunkOffsets(t, head)
	if !wide[0] || !wide[1] {
		t.Fatalf("tables %v, want every stco a co64", wide)
	}
	for i, old := range [][]int64{high, low} {
		for j, from := range old {
			to := tracks[i][j]
			if to != from+grown {
				t.Fatalf("track %d chunk %d at %d, want %d", i, j, to, from+grown)
			}
			want := chunks[from]
			got, err := layout.Read(ctx, f, to, int64(len(want)))
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("track %d chunk %d: %q at %d, err %v", i, j, got, to, err)
			}
		}
	}
	if tracks[0][1] <= 0xFFFF_FFFF {
		t.Fatalf("the high chunk lands at %d, under 4 GiB: the test proves nothing", tracks[0][1])
	}
}
