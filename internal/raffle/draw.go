package raffle

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"slices"
	"strings"

	"github.com/google/uuid"
)

// Algorithm names the draw below; a verifier refuses any other.
const Algorithm = "sky-raffle-v1"

// SeedSize is the seed's length in bytes.
const SeedSize = 32

// SortedIDs are ids as the list is hashed: lowercase canonical form, in
// ascending order, each once.
func SortedIDs(ids []uuid.UUID) []uuid.UUID {
	out := slices.Clone(ids)
	slices.SortFunc(out, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	return slices.Compact(out)
}

// ListHash is the SHA-256 of the sorted ids, one per line (no newline at
// the end).
func ListHash(sorted []uuid.UUID) [32]byte {
	parts := make([]string, len(sorted))
	for i, id := range sorted {
		parts[i] = id.String()
	}
	return sha256.Sum256([]byte(strings.Join(parts, "\n")))
}

// NewSeed draws a seed from crypto/rand.
func NewSeed() ([SeedSize]byte, error) {
	var seed [SeedSize]byte
	_, err := rand.Read(seed[:])
	return seed, err
}

// stream is the draw's deterministic number source: block n is
// SHA-256(seed ‖ listHash ‖ n as 8 big-endian bytes), and each number is a
// block's first 8 bytes, big-endian.
type stream struct {
	seed, list [32]byte
	counter    uint64
}

func (s *stream) next() uint64 {
	var buf [72]byte
	copy(buf[:32], s.seed[:])
	copy(buf[32:64], s.list[:])
	binary.BigEndian.PutUint64(buf[64:], s.counter)
	s.counter++
	sum := sha256.Sum256(buf[:])
	return binary.BigEndian.Uint64(sum[:8])
}

// uniform is a number in [0, m) without modulo bias: a number at or above
// the largest multiple of m that fits in 64 bits is drawn again.
func (s *stream) uniform(m uint64) uint64 {
	// 2^64 mod m, in 64-bit arithmetic.
	rem := (-m) % m
	limit := -rem // 2^64 − rem (0 when rem is 0: every number is fine)
	for {
		x := s.next()
		if rem == 0 || x < limit {
			return x % m
		}
	}
}

// Order draws k of sorted (the eligible Tickets, as SortedIDs) by seed: a
// Fisher–Yates shuffle stopped after k places. The first places win.
func Order(sorted []uuid.UUID, seed [32]byte, k int) []uuid.UUID {
	a := slices.Clone(sorted)
	k = min(k, len(a))
	s := &stream{seed: seed, list: ListHash(sorted)}
	for i := 0; i < k; i++ {
		j := i + int(s.uniform(uint64(len(a)-i)))
		a[i], a[j] = a[j], a[i]
	}
	return a[:k]
}

// ErrVerify is a verification that does not hold.
var ErrVerify = errors.New("raffle: verification does not hold")

// Verify draws v's order again from its list and seed: its list's hash and
// its order must be what was kept.
func Verify(v Verification) error {
	if v.Algorithm != Algorithm {
		return ErrVerify
	}
	sorted := SortedIDs(v.TicketIDs)
	if len(sorted) != len(v.TicketIDs) {
		return ErrVerify
	}
	sum := ListHash(sorted)
	if hex.EncodeToString(sum[:]) != v.ListHash {
		return ErrVerify
	}
	raw, err := hex.DecodeString(v.Seed)
	if err != nil || len(raw) != SeedSize {
		return ErrVerify
	}
	var seed [32]byte
	copy(seed[:], raw)
	if !slices.Equal(Order(sorted, seed, len(v.Order)), v.Order) {
		return ErrVerify
	}
	return nil
}
