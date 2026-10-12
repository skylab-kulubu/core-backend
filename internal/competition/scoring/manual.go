package scoring

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"

	"github.com/google/uuid"
)

// Manual is the score an organizer types in (AGC's weekly 30, 50, 60): the
// sum of an Entrant's manual rows. It takes no setting yet; score columns
// ("Tur 1", "Tur 2") come with a later ticket.
type Manual struct{}

func (Manual) Key() string { return SourceManual }

func (Manual) Validate(config json.RawMessage) error {
	trimmed := bytes.TrimSpace(config)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &fields); err != nil || fields == nil {
		return fmt.Errorf("%w: manual takes an empty object", ErrInvalidConfig)
	}
	if len(fields) != 0 {
		return fmt.Errorf("%w: manual takes no settings yet", ErrInvalidConfig)
	}
	return nil
}

// errOverflow is a sum past what Points holds.
var errOverflow = errors.New("scoring: score overflows")

func (Manual) Standings(in Input) ([]Standing, error) {
	active := make(map[uuid.UUID]bool, len(in.Entrants))
	for _, id := range in.Entrants {
		active[id] = true
	}
	sums := map[uuid.UUID]Points{}
	for _, e := range in.Entries {
		if e.Source != SourceManual || !active[e.EntrantID] {
			continue
		}
		if !in.AsOf.IsZero() && e.OccurredAt.After(in.AsOf) {
			continue
		}
		sum, ok := add(sums[e.EntrantID], e.Value)
		if !ok {
			return nil, errOverflow
		}
		sums[e.EntrantID] = sum
	}
	out := make([]Standing, 0, len(sums))
	for id, score := range sums {
		out = append(out, Standing{EntrantID: id, Score: score})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].EntrantID.String() < out[j].EntrantID.String() })
	return out, nil
}

func add(a, b Points) (Points, bool) {
	if (b > 0 && a > math.MaxInt64-b) || (b < 0 && a < math.MinInt64-b) {
		return 0, false
	}
	return a + b, true
}
