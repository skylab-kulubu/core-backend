package scoring

import (
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/google/uuid"
)

// Sources of a ledger row (spec §5.5). An import's source is "import:<batch>"
// (ticket 12).
const (
	// SourceManual is a score an organizer typed in.
	SourceManual = "manual"
	// SourceNative is a submission made on the platform (tickets 16, 19).
	SourceNative = "native"
)

// ErrInvalidConfig is a Stage setting the strategy does not accept.
var ErrInvalidConfig = errors.New("scoring: invalid strategy config")

// Entry is one ledger row a strategy reads: never a revoked one, the caller
// leaves those out.
type Entry struct {
	EntrantID  uuid.UUID
	Source     string
	Value      Points
	OccurredAt time.Time
}

// Input is a Stage's ledger at one moment.
type Input struct {
	// Config is the Stage's strategy setting, as Validate accepted it.
	Config json.RawMessage
	// Entrants are the active Entrants; a row of anyone else never counts.
	Entrants []uuid.UUID
	// Entries are the Stage's rows that are not revoked.
	Entries []Entry
	// AsOf leaves out every row that occurred after it. Zero leaves out
	// nothing.
	AsOf time.Time
}

// Standing is one Entrant's computed score.
type Standing struct {
	EntrantID uuid.UUID
	Score     Points
	// TieKeys break equal scores in order, the lower one ranking first.
	// Equal score and equal keys share a rank.
	TieKeys []int64
}

// Strategy computes a Stage's standings from its ledger. A new kind of
// scoring is a new Strategy here and a line in registry; there are no
// runtime plugins (ADR-0068).
type Strategy interface {
	Key() string
	// Validate checks a Stage's setting before it is saved.
	Validate(config json.RawMessage) error
	// Standings is pure: the same Input gives the same standings, whatever
	// the order of its slices. It ranks nothing; Rank does.
	Standings(in Input) ([]Standing, error)
}

var registry = map[string]Strategy{
	SourceManual: Manual{},
}

// Lookup is the strategy with key.
func Lookup(key string) (Strategy, bool) {
	s, ok := registry[key]
	return s, ok
}

// Keys are the registered strategies, sorted.
func Keys() []string {
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Ranked is a Standing with its rank.
type Ranked struct {
	Standing
	Rank int
}

// Rank orders standings by score (highest first), then by their tie keys,
// and gives equal score and keys the same rank: 1, 1, 3 (standard
// competition ranking, as core and the Java era did). Entrants that share a
// rank are listed by id, so the order never depends on the input's.
func Rank(standings []Standing) []Ranked {
	out := make([]Ranked, len(standings))
	for i, s := range standings {
		out[i] = Ranked{Standing: s}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if c := compare(out[i].Standing, out[j].Standing); c != 0 {
			return c < 0
		}
		return out[i].EntrantID.String() < out[j].EntrantID.String()
	})
	for i := range out {
		if i > 0 && compare(out[i].Standing, out[i-1].Standing) == 0 {
			out[i].Rank = out[i-1].Rank
			continue
		}
		out[i].Rank = i + 1
	}
	return out
}

// compare is negative when a ranks before b, zero when they share a rank.
func compare(a, b Standing) int {
	if a.Score != b.Score {
		if a.Score > b.Score {
			return -1
		}
		return 1
	}
	for i := 0; i < len(a.TieKeys) || i < len(b.TieKeys); i++ {
		switch {
		case i >= len(a.TieKeys):
			return 1
		case i >= len(b.TieKeys):
			return -1
		case a.TieKeys[i] < b.TieKeys[i]:
			return -1
		case a.TieKeys[i] > b.TieKeys[i]:
			return 1
		}
	}
	return 0
}
