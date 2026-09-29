package faststart_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/faststart"
)

// withMoov is an ftyp, an mdat of two chunks and the moov box made from
// their offsets, in that order.
func withMoov(makeMoov func(offsets []uint32) []byte) []byte {
	file := ftyp()
	a, b := chunk("v0", 40), chunk("v1", 40)
	offsets := []uint32{uint32(len(file) + 8), uint32(len(file) + 8 + len(a))}
	file = append(file, box("mdat", a, b)...)
	return append(file, makeMoov(offsets)...)
}

func oneTrack(offsets []uint32) []byte { return moov(trak(1, stco(offsets...))) }

// stbl is a trak whose sample table holds the boxes given.
func trakWith(stblBoxes ...[]byte) []byte {
	return box("trak", box("mdia", box("minf", box("stbl", stblBoxes...))))
}

// A file the rewrite cannot move safely is refused, with a reason, and
// never half rewritten: a malformed box, a moov too large to read, what a
// fragmented or compressed file holds, a chunk offset outside the media
// data. It still plays once downloaded whole.
func TestPlanRefusesWhatItCannotMoveSafely(t *testing.T) {
	t.Parallel()
	good := withMoov(oneTrack)
	small := faststart.DefaultLimits
	for name, c := range map[string]struct {
		file   []byte
		limits faststart.Limits
	}{
		"a moov over the limit":         {good, faststart.Limits{MaxMoovBytes: 64, MaxTopLevelBoxes: 256, MaxMoovBoxes: 100}},
		"a cut-short file":              {good[:len(good)-10], small},
		"stray bytes at the end":        {append(append([]byte{}, good...), 0, 0, 0), small},
		"a box shorter than its header": {append(append([]byte{}, good...), append(be32(4), "free"...)...), small},
		"a 64-bit size past any file":   {append(append(append([]byte{}, good...), be32(1)...), append([]byte("free"), be64(1<<63)...)...), small},
		"an unprintable top-level type": {append(append([]byte{}, good...), box("fr\x00e")...), small},
		"no ftyp first":                 {append(box("free"), good...), small},
		"no moov":                       {append(ftyp(), box("mdat", chunk("v0", 40))...), small},
		"two moov boxes":                {append(append([]byte{}, good...), oneTrack([]uint32{36})...), small},
		"too many top-level boxes":      {good, faststart.Limits{MaxMoovBytes: 1 << 20, MaxTopLevelBoxes: 2, MaxMoovBoxes: 100}},
		"too many boxes in the moov":    {good, faststart.Limits{MaxMoovBytes: 1 << 20, MaxTopLevelBoxes: 256, MaxMoovBoxes: 5}},
		"a fragment after the moov":     {append(append([]byte{}, good...), box("moof", fullBox("mfhd", 0, 0, be32(1)))...), small},
		"a fragmented moov (mvex)": {withMoov(func(o []uint32) []byte {
			return moov(trak(1, stco(o...)), box("mvex", fullBox("trex", 0, 0, make([]byte, 20))))
		}), small},
		"a compressed moov (cmov)": {withMoov(func(o []uint32) []byte { return moov(box("cmov", make([]byte, 12))) }), small},
		"sample auxiliary offsets": {withMoov(func(o []uint32) []byte {
			return moov(trakWith(fullBox("saio", 0, 0, be32(1), be32(o[0])), stco(o...)))
		}), small},
		"a size-0 box in the moov": {withMoov(func(o []uint32) []byte {
			return moov(trakWith(stco(o...)), append(be32(0), "udta"...))
		}), small},
		"an stco whose count is not its size": {withMoov(func(o []uint32) []byte {
			table := stco(o...)
			binary.BigEndian.PutUint32(table[12:], 3)
			return moov(trakWith(table))
		}), small},
		"a chunk offset outside the media data": {withMoov(func(o []uint32) []byte { return oneTrack([]uint32{o[0], 4}) }), small},
		"no chunk offsets":                      {withMoov(func([]uint32) []byte { return moov(trakWith(fullBox("stsz", 0, 0, be32(0), be32(0)))) }), small},
		"stray bytes in a walked box": {withMoov(func(o []uint32) []byte {
			return moov(box("trak", box("mdia", box("minf", box("stbl", stco(o...)), []byte{1, 2, 3}))))
		}), small},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := faststart.Plan(context.Background(), memory(c.file), int64(len(c.file)), c.limits)
			var refusal *faststart.Refusal
			if !errors.Is(err, faststart.ErrInvalid) || !errors.As(err, &refusal) || refusal.Reason == "" {
				t.Fatalf("err = %v, want a refusal", err)
			}
			t.Log(refusal.Reason)
		})
	}
}

// A QuickTime terminator (zero bytes after a walked box's last child) is
// kept, and so is a moov whose size runs to the end of the file (size 0).
func TestPlanKeepsTerminatorsAndAMoovToTheEnd(t *testing.T) {
	t.Parallel()
	file := withMoov(func(o []uint32) []byte {
		m := moov(box("trak", box("mdia", box("minf", box("stbl", stco(o...)), []byte{0, 0, 0, 0}))))
		binary.BigEndian.PutUint32(m, 0)
		return m
	})
	out, _ := rewrite(t, file)
	if len(out) != len(file) {
		t.Fatalf("rewritten %d bytes, want %d", len(out), len(file))
	}
	if !bytes.Contains(out, []byte("stbl")) || !bytes.Equal(out[len(ftyp())+4:len(ftyp())+8], []byte("moov")) {
		t.Fatal("the moov is not first after the ftyp")
	}
	if got := binary.BigEndian.Uint32(out[len(ftyp()):]); got == 0 {
		t.Fatal("the moved moov kept a size of 0: it would swallow the media data")
	}
}

// A moov whose rewrite (an stco upgraded to a co64) would pass the limit is
// refused too.
func TestPlanRefusesAnUpgradeOverTheLimit(t *testing.T) {
	t.Parallel()
	f := &sparse{}
	f.place(0, ftyp())
	mdatAt := int64(len(ftyp()))
	mdatEnd := int64(0xFFFF_FFE0)
	f.place(mdatAt, append(be32(1), append([]byte("mdat"), be64(uint64(mdatEnd-mdatAt))...)...))
	moovBox := moov(trak(1, stco(0xFFFF_FF80, 0xFFFF_FFB0)))
	f.place(mdatEnd, moovBox)
	f.size = mdatEnd + int64(len(moovBox))
	limits := faststart.Limits{MaxMoovBytes: int64(len(moovBox)) + 4, MaxTopLevelBoxes: 8, MaxMoovBoxes: 100}
	if _, err := faststart.Plan(context.Background(), f, f.size, limits); !errors.Is(err, faststart.ErrInvalid) {
		t.Fatalf("err = %v, want a refusal", err)
	}
}
