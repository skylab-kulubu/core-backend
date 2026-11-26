package raffle

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"
)

// vectorIDs, vectorSeed and the answers below are the draw's shared test
// vector: the panel's verifier (core-frontend src/lib/raffle/verify.ts)
// checks the same one, so the two draw alike.
var vectorIDs = []string{
	"9c0d1e2f-3a4b-4c5d-8e6f-7a8b9c0d1e2f",
	"0b6f3c1e-4d2a-4f7e-9a51-1c2d3e4f5a61",
	"1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d",
	"2f9e8d7c-6b5a-4c3d-9e2f-1a0b9c8d7e6f",
	"3c4d5e6f-7a8b-4c9d-8e0f-1a2b3c4d5e6f",
	"4e5f6a7b-8c9d-4e0f-9a1b-2c3d4e5f6a7b",
	"5a6b7c8d-9e0f-4a1b-8c2d-3e4f5a6b7c8d",
	"6c7d8e9f-0a1b-4c2d-9e3f-4a5b6c7d8e9f",
	"7e8f9a0b-1c2d-4e3f-8a4b-5c6d7e8f9a0b",
	"8a9b0c1d-2e3f-4a4b-9c5d-6e7f8a9b0c1d",
}

const (
	vectorSeed     = "8f1c2b7a9d3e4f5061728394a5b6c7d8e9f00112233445566778899aabbccdde"
	vectorListHash = "640cfca901485c396ae9b9c63c423e026a8410dda2a763eed8323be801b6c85b"
)

var vectorOrder = []string{
	"8a9b0c1d-2e3f-4a4b-9c5d-6e7f8a9b0c1d",
	"6c7d8e9f-0a1b-4c2d-9e3f-4a5b6c7d8e9f",
	"2f9e8d7c-6b5a-4c3d-9e2f-1a0b9c8d7e6f",
	"7e8f9a0b-1c2d-4e3f-8a4b-5c6d7e8f9a0b",
	"5a6b7c8d-9e0f-4a1b-8c2d-3e4f5a6b7c8d",
}

func parseIDs(t *testing.T, raw []string) []uuid.UUID {
	t.Helper()
	out := make([]uuid.UUID, len(raw))
	for i, s := range raw {
		out[i] = uuid.MustParse(s)
	}
	return out
}

func vectorSeedBytes(t *testing.T) [32]byte {
	t.Helper()
	raw, err := hex.DecodeString(vectorSeed)
	if err != nil || len(raw) != 32 {
		t.Fatal("bad seed")
	}
	var seed [32]byte
	copy(seed[:], raw)
	return seed
}

func TestDrawMatchesTheSharedVector(t *testing.T) {
	sorted := SortedIDs(parseIDs(t, vectorIDs))
	sum := ListHash(sorted)
	if got := hex.EncodeToString(sum[:]); got != vectorListHash {
		t.Fatalf("list hash %s", got)
	}
	order := Order(sorted, vectorSeedBytes(t), 5)
	if !slices.Equal(order, parseIDs(t, vectorOrder)) {
		t.Fatalf("order %v", order)
	}
	// The list is hashed sorted: the order it came in changes nothing.
	shuffled := parseIDs(t, vectorIDs)
	slices.Reverse(shuffled)
	if !slices.Equal(Order(SortedIDs(shuffled), vectorSeedBytes(t), 5), order) {
		t.Fatal("order depends on the input's order")
	}
	// The first places are the same however many are drawn.
	if !slices.Equal(Order(sorted, vectorSeedBytes(t), 2), order[:2]) {
		t.Fatal("a shorter draw is not the longer one's head")
	}
}

func TestVerifyHoldsAndCatchesTampering(t *testing.T) {
	sorted := SortedIDs(parseIDs(t, vectorIDs))
	v := Verification{Algorithm: Algorithm, Seed: vectorSeed, ListHash: vectorListHash, TicketIDs: sorted, Order: parseIDs(t, vectorOrder)}
	if err := Verify(v); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*Verification){
		"another seed":     func(v *Verification) { v.Seed = vectorSeed[:62] + "00" },
		"a Ticket added":   func(v *Verification) { v.TicketIDs = append(slices.Clone(v.TicketIDs), uuid.New()) },
		"a Ticket dropped": func(v *Verification) { v.TicketIDs = v.TicketIDs[1:] },
		"winners swapped": func(v *Verification) {
			v.Order = []uuid.UUID{v.Order[1], v.Order[0], v.Order[2], v.Order[3], v.Order[4]}
		},
		"another algorithm": func(v *Verification) { v.Algorithm = "sky-raffle-v0" },
		"a duplicate":       func(v *Verification) { v.TicketIDs = append(slices.Clone(v.TicketIDs), v.TicketIDs[0]) },
	}
	for name, tamper := range cases {
		bad := v
		bad.TicketIDs, bad.Order = slices.Clone(v.TicketIDs), slices.Clone(v.Order)
		tamper(&bad)
		if err := Verify(bad); !errors.Is(err, ErrVerify) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Every Ticket is as likely to win, and to win each place: over many
// seeds, a chi-square test of the first and the last drawn place against
// the uniform count. 20 000 draws of 3 among 10; the 0.999 quantile of
// chi-square with 9 degrees of freedom is 27.88.
func TestDrawIsUniform(t *testing.T) {
	ids := make([]uuid.UUID, 10)
	for i := range ids {
		ids[i] = uuid.MustParse("00000000-0000-4000-8000-00000000000" + string(rune('0'+i)))
	}
	sorted := SortedIDs(ids)
	index := map[uuid.UUID]int{}
	for i, id := range sorted {
		index[id] = i
	}
	const draws = 20000
	first, third := make([]int, len(sorted)), make([]int, len(sorted))
	pair := map[[2]int]int{}
	for n := 0; n < draws; n++ {
		var buf [8]byte
		binary.BigEndian.PutUint64(buf[:], uint64(n))
		seed := sha256.Sum256(append([]byte("raffle-uniformity"), buf[:]...))
		order := Order(sorted, seed, 3)
		if len(order) != 3 || order[0] == order[1] || order[1] == order[2] || order[0] == order[2] {
			t.Fatalf("draw %d: %v", n, order)
		}
		first[index[order[0]]]++
		third[index[order[2]]]++
		pair[[2]int{index[order[0]], index[order[1]]}]++
	}
	chi := func(counts []int) float64 {
		want := float64(draws) / float64(len(counts))
		sum := 0.0
		for _, c := range counts {
			d := float64(c) - want
			sum += d * d / want
		}
		return sum
	}
	if c := chi(first); c > 27.88 {
		t.Errorf("first place chi-square %.2f: %v", c, first)
	}
	if c := chi(third); c > 27.88 {
		t.Errorf("third place chi-square %.2f: %v", c, third)
	}
	// Ordered pairs (90 of them) too; the 0.999 quantile with 89 degrees
	// of freedom is 135.98.
	counts := make([]int, 0, 90)
	for a := range 10 {
		for b := range 10 {
			if a != b {
				counts = append(counts, pair[[2]int{a, b}])
			}
		}
	}
	if c := chi(counts); c > 135.98 {
		t.Errorf("pair chi-square %.2f", c)
	}
}

func TestUniformHasNoModuloBias(t *testing.T) {
	// m = 3 does not divide 2^64: the stream must reject the top numbers.
	// Its answers stay in range whatever the seed.
	s := &stream{}
	for i := range 1000 {
		s.seed[0] = byte(i)
		if v := s.uniform(3); v >= 3 {
			t.Fatalf("uniform(3) = %d", v)
		}
	}
	if v := (&stream{}).uniform(1); v != 0 {
		t.Fatalf("uniform(1) = %d", v)
	}
	// A power of two takes every number.
	if v := (&stream{}).uniform(1 << 63); v >= 1<<63 {
		t.Fatalf("uniform(2^63) = %d", v)
	}
}

func TestNewSeedIsRandom(t *testing.T) {
	a, err := NewSeed()
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewSeed()
	if err != nil {
		t.Fatal(err)
	}
	if a == b || a == [32]byte{} {
		t.Fatal("seeds repeat")
	}
}

func TestMasks(t *testing.T) {
	for in, want := range map[string]string{
		"Ayşe Yılmaz": "Ay*** Yı***", "Ö": "Ö***", "": "", "  Can  Öz  ": "Ca*** Öz***",
	} {
		if got := MaskName(in); got != want {
			t.Errorf("MaskName(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"ayse@gmail.com": "a***@g***.com", "x@std.yildiz.edu.tr": "x***@s***.tr", "bad": "", "@a.com": "",
	} {
		if got := MaskEmail(in); got != want {
			t.Errorf("MaskEmail(%q) = %q, want %q", in, got, want)
		}
	}
}
