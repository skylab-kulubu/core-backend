package scoring

import (
	"encoding/json"
	"errors"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestParsePoints(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]Points{
		"30": 30_000, "0": 0, "-2": -2_000, "50.5": 50_500, "0.125": 125, "-0.001": -1, "60.10": 60_100,
		"1000000000": MaxPoints,
	} {
		got, err := ParsePoints(raw)
		if err != nil || got != want {
			t.Errorf("ParsePoints(%q) = %d, %v; want %d", raw, got, err, want)
		}
	}
	for _, raw := range []string{"", "-", "1.2345", "1e3", "abc", "1.", ".5", "+3", "1000000000.001", "-1000000000.001", "99999999999999999999", "1 "} {
		if got, err := ParsePoints(raw); !errors.Is(err, ErrInvalidPoints) {
			t.Errorf("ParsePoints(%q) = %d, %v; want ErrInvalidPoints", raw, got, err)
		}
	}
}

func TestPointsJSON(t *testing.T) {
	t.Parallel()
	for p, want := range map[Points]string{30_000: "30", 50_500: "50.5", 125: "0.125", -1: "-0.001", 0: "0", -2_100: "-2.1"} {
		got, err := json.Marshal(p)
		if err != nil || string(got) != want {
			t.Errorf("Marshal(%d) = %s, %v; want %s", p, got, err, want)
		}
		var back Points
		if err := json.Unmarshal(got, &back); err != nil || back != p {
			t.Errorf("Unmarshal(%s) = %d, %v", got, back, err)
		}
	}
	var p Points
	for _, raw := range []string{`"30"`, `null`, `1e3`, `true`, `30.0001`} {
		if err := json.Unmarshal([]byte(raw), &p); err == nil {
			t.Errorf("Unmarshal(%s) accepted", raw)
		}
	}
}

func TestLookupKnowsOnlyManualYet(t *testing.T) {
	t.Parallel()
	if s, ok := Lookup("manual"); !ok || s.Key() != "manual" {
		t.Fatalf("manual = %v, %v", s, ok)
	}
	if _, ok := Lookup("problems"); ok {
		t.Fatal("problems strategy registered before ticket 10")
	}
	if got := Keys(); !slices.Equal(got, []string{"manual"}) {
		t.Fatalf("Keys() = %v", got)
	}
}

func TestManualValidate(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{``, `null`, `{}`, ` { } `} {
		if err := (Manual{}).Validate(json.RawMessage(ok)); err != nil {
			t.Errorf("Validate(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{`[]`, `{"columns":["Tur 1"]}`, `3`, `{`} {
		if err := (Manual{}).Validate(json.RawMessage(bad)); !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("Validate(%q) = %v, want ErrInvalidConfig", bad, err)
		}
	}
}

func ids(n int) []uuid.UUID {
	out := make([]uuid.UUID, n)
	for i := range out {
		out[i] = uuid.New()
	}
	return out
}

func TestManualSumsManualEntriesOfActiveEntrants(t *testing.T) {
	t.Parallel()
	e := ids(4)
	at := time.Date(2026, 10, 12, 18, 0, 0, 0, time.UTC)
	in := Input{
		Entrants: e[:3],
		Entries: []Entry{
			{EntrantID: e[0], Source: SourceManual, Value: 30_000, OccurredAt: at},
			{EntrantID: e[0], Source: SourceManual, Value: 20_500, OccurredAt: at},
			{EntrantID: e[1], Source: SourceManual, Value: 60_000, OccurredAt: at},
			// Not a manual row: the manual strategy does not read it.
			{EntrantID: e[1], Source: "native", Value: 1_000_000, OccurredAt: at},
			// e[2] has no entry: not in the standings.
			// e[3] is not an active Entrant: never counted.
			{EntrantID: e[3], Source: SourceManual, Value: 90_000, OccurredAt: at},
		},
	}
	got, err := Manual{}.Standings(in)
	if err != nil {
		t.Fatal(err)
	}
	want := map[uuid.UUID]Points{e[0]: 50_500, e[1]: 60_000}
	if len(got) != len(want) {
		t.Fatalf("standings %+v", got)
	}
	for _, s := range got {
		if want[s.EntrantID] != s.Score || len(s.TieKeys) != 0 {
			t.Errorf("standing %+v, want score %d", s, want[s.EntrantID])
		}
	}
}

// A row recorded after AsOf never counts (the freeze of ticket 13 builds
// on it).
func TestManualIgnoresEntriesAfterAsOf(t *testing.T) {
	t.Parallel()
	e := ids(1)
	asOf := time.Date(2026, 10, 12, 18, 0, 0, 0, time.UTC)
	got, err := Manual{}.Standings(Input{
		Entrants: e,
		AsOf:     asOf,
		Entries: []Entry{
			{EntrantID: e[0], Source: SourceManual, Value: 30_000, OccurredAt: asOf},
			{EntrantID: e[0], Source: SourceManual, Value: 20_000, OccurredAt: asOf.Add(time.Second)},
		},
	})
	if err != nil || len(got) != 1 || got[0].Score != 30_000 {
		t.Fatalf("standings %+v, %v", got, err)
	}
}

func TestManualRefusesOverflow(t *testing.T) {
	t.Parallel()
	e := ids(1)
	entries := make([]Entry, 0, 10)
	for range 10 {
		entries = append(entries, Entry{EntrantID: e[0], Source: SourceManual, Value: Points(1 << 62)})
	}
	if _, err := (Manual{}).Standings(Input{Entrants: e, Entries: entries}); err == nil {
		t.Fatal("overflowing sum accepted")
	}
}

func TestRankSharesRankOnEqualScores(t *testing.T) {
	t.Parallel()
	e := ids(5)
	got := Rank([]Standing{
		{EntrantID: e[0], Score: 30_000},
		{EntrantID: e[1], Score: 60_000},
		{EntrantID: e[2], Score: 60_000},
		{EntrantID: e[3], Score: 50_000},
		{EntrantID: e[4], Score: 30_000},
	})
	ranks := make([]int, len(got))
	scores := make([]Points, len(got))
	for i, r := range got {
		ranks[i], scores[i] = r.Rank, r.Score
	}
	if !slices.Equal(ranks, []int{1, 1, 3, 4, 4}) || !slices.Equal(scores, []Points{60_000, 60_000, 50_000, 30_000, 30_000}) {
		t.Fatalf("ranks %v scores %v", ranks, scores)
	}
}

func TestRankReadsTieKeysAfterTheScore(t *testing.T) {
	t.Parallel()
	e := ids(3)
	got := Rank([]Standing{
		{EntrantID: e[0], Score: 3, TieKeys: []int64{120, 50}},
		{EntrantID: e[1], Score: 3, TieKeys: []int64{100, 90}},
		{EntrantID: e[2], Score: 3, TieKeys: []int64{120, 50}},
	})
	if got[0].EntrantID != e[1] || got[0].Rank != 1 || got[1].Rank != 2 || got[2].Rank != 2 {
		t.Fatalf("ranked %+v", got)
	}
}

// The input's order never changes the result.
func TestRankAndManualDoNotDependOnInputOrder(t *testing.T) {
	t.Parallel()
	e := ids(8)
	var entries []Entry
	for i, id := range e {
		entries = append(entries, Entry{EntrantID: id, Source: SourceManual, Value: Points((i % 3) * 10_000)})
	}
	want, err := Manual{}.Standings(Input{Entrants: e, Entries: entries})
	if err != nil {
		t.Fatal(err)
	}
	wantRanked := Rank(want)
	r := rand.New(rand.NewPCG(1, 2))
	for range 20 {
		shuffledE := slices.Clone(e)
		shuffledEntries := slices.Clone(entries)
		r.Shuffle(len(shuffledE), func(i, j int) { shuffledE[i], shuffledE[j] = shuffledE[j], shuffledE[i] })
		r.Shuffle(len(shuffledEntries), func(i, j int) { shuffledEntries[i], shuffledEntries[j] = shuffledEntries[j], shuffledEntries[i] })
		got, err := Manual{}.Standings(Input{Entrants: shuffledE, Entries: shuffledEntries})
		if err != nil {
			t.Fatal(err)
		}
		gotRanked := Rank(got)
		if len(gotRanked) != len(wantRanked) {
			t.Fatalf("len %d want %d", len(gotRanked), len(wantRanked))
		}
		for i := range gotRanked {
			if gotRanked[i].EntrantID != wantRanked[i].EntrantID || gotRanked[i].Rank != wantRanked[i].Rank || gotRanked[i].Score != wantRanked[i].Score {
				t.Fatalf("order-dependent result at %d: %+v vs %+v", i, gotRanked[i], wantRanked[i])
			}
		}
	}
}
