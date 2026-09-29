package faststart_test

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/faststart"
	"github.com/skylab-kulubu/core-backend/internal/faststart/mp4test"
)

// A camera's MP4 has its moov after the media data. The rewrite puts the
// moov in front of it, and shifts every chunk offset of every track by the
// moov's size: each chunk's bytes are where its track's table now says.
func TestPlanMovesTheMoovInFrontOfTheMediaData(t *testing.T) {
	t.Parallel()
	m := mp4test.TwoTracks()
	file, _ := m.Build()
	rewritten, layout := rewrite(t, file)
	if layout.Size != int64(len(file)) || len(rewritten) != len(file) {
		t.Fatalf("rewritten %d bytes (layout %d), want %d: an stco that fits keeps its size", len(rewritten), layout.Size, len(file))
	}
	if got := mp4test.Types(mp4test.Boxes(t, rewritten, 0, int64(len(rewritten)))); len(got) != 3 || got[0] != "ftyp" || got[1] != "moov" || got[2] != "mdat" {
		t.Fatalf("top-level boxes %v, want [ftyp moov mdat]", got)
	}
	mp4test.AssertSamePlayback(t, m, rewritten)
	_, wide := mp4test.ChunkOffsets(t, rewritten)
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
	m := mp4test.TwoTracks()
	file, offsets := m.Build()
	out, _ := rewrite(t, file)
	for name, f := range map[string][]byte{
		"moov first":    out,
		"no media data": append(mp4test.Ftyp(), m.Moov(offsets)...),
	} {
		_, err := faststart.Plan(context.Background(), mp4test.Memory(f), int64(len(f)), faststart.DefaultLimits)
		if !errors.Is(err, faststart.ErrAlreadyFaststart) {
			t.Errorf("%s: %v, want ErrAlreadyFaststart", name, err)
		}
	}
}

// 64-bit chunk offsets (co64) and 64-bit box sizes move the same way.
func TestPlanMovesCo64AndLargeBoxes(t *testing.T) {
	t.Parallel()
	m := mp4test.TwoTracks()
	m.Wide = true
	m.LargeMdat = true
	file, _ := m.Build()
	rewritten, _ := rewrite(t, file)
	mp4test.AssertSamePlayback(t, m, rewritten)
	if _, wide := mp4test.ChunkOffsets(t, rewritten); !wide[0] || !wide[1] {
		t.Fatalf("co64 tables became %v", wide)
	}
	mdat := mp4test.Boxes(t, rewritten, 0, int64(len(rewritten)))[2]
	if mdat.Type != "mdat" || mdat.Header != 16 {
		t.Fatalf("the mdat is %q with a %d-byte header, want its 64-bit size kept", mdat.Type, mdat.Header)
	}
}

// Boxes between the ftyp and the media data stay in front of the moov;
// boxes after the old moov stay at the end, and a chunk in an mdat after
// it moves with it.
func TestPlanKeepsTheOtherBoxesInPlace(t *testing.T) {
	t.Parallel()
	m := mp4test.TwoTracks()
	m.Before = [][]byte{mp4test.Box("free", make([]byte, 20)), mp4test.Box("uuid", make([]byte, 16), []byte("xmp"))}
	file, offsets := m.Build()
	late := mp4test.Chunk("v3", 32)
	file = append(file, mp4test.Box("free", make([]byte, 4))...)
	lateAt := int64(len(file) + 8)
	file = append(file, mp4test.Box("mdat", late)...)
	// Point track 0's last chunk at the mdat after the moov: the moov is
	// rebuilt with that offset, at the same size.
	m.Chunks = append(m.Chunks, late)
	m.Tracks[0] = append(m.Tracks[0][:2], len(m.Chunks)-1)
	offsets = append(offsets, lateAt)
	head := bytes.Index(file, []byte("moov")) - 4
	moovBox := m.Moov(offsets)
	file = append(append(append([]byte{}, file[:head]...), moovBox...), file[head+len(moovBox):]...)

	rewritten, _ := rewrite(t, file)
	got := mp4test.Types(mp4test.Boxes(t, rewritten, 0, int64(len(rewritten))))
	want := []string{"ftyp", "free", "uuid", "moov", "mdat", "free", "mdat"}
	if !slices.Equal(got, want) {
		t.Fatalf("top-level boxes %v, want %v", got, want)
	}
	mp4test.AssertSamePlayback(t, m, rewritten)
}

// An stco whose offsets would pass 4 GiB once moved becomes a co64, as
// qt-faststart does: every stco of the moov at once, the moov growing by
// four bytes an entry and the shift with it. The file is a sparse one of a
// little over 4 GiB, read only where the rewrite looks.
func TestPlanUpgradesAnStcoThatWouldOverflow(t *testing.T) {
	t.Parallel()
	high := []int64{0xFFFF_FF80, 0xFFFF_FFB0}
	low := []int64{100, 200}
	chunks := map[int64][]byte{high[0]: mp4test.Chunk("v0", 40), high[1]: mp4test.Chunk("v1", 40), low[0]: mp4test.Chunk("a0", 24), low[1]: mp4test.Chunk("a1", 24)}
	f := &mp4test.Sparse{}
	f.Place(0, mp4test.Ftyp())
	mdatAt := int64(len(mp4test.Ftyp()))
	mdatEnd := int64(0xFFFF_FFE0)
	f.Place(mdatAt, append(mp4test.U32(1), append([]byte("mdat"), mp4test.U64(uint64(mdatEnd-mdatAt))...)...))
	for at, c := range chunks {
		f.Place(at, c)
	}
	moovBox := mp4test.Moov(
		mp4test.Trak(1, mp4test.Stco(uint32(high[0]), uint32(high[1]))),
		mp4test.Trak(2, mp4test.Stco(uint32(low[0]), uint32(low[1]))),
	)
	f.Place(mdatEnd, moovBox)
	f.Size = mdatEnd + int64(len(moovBox))

	ctx := context.Background()
	layout, err := faststart.Plan(ctx, f, f.Size, faststart.DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	grown := int64(len(moovBox)) + 4*4
	if layout.Size != f.Size+16 || layout.MoovAt != mdatAt {
		t.Fatalf("rewritten %d bytes with the moov at %d, want %d and %d", layout.Size, layout.MoovAt, f.Size+16, mdatAt)
	}
	head, err := layout.Read(ctx, f, 0, layout.MoovAt+grown)
	if err != nil {
		t.Fatal(err)
	}
	tracks, wide := mp4test.ChunkOffsets(t, head)
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
