package optional_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/skylab-kulubu/core-backend/internal/optional"
)

type body struct {
	Name  optional.Field[string]     `json:"name"`
	Count optional.Field[int]        `json:"count"`
	At    optional.Field[*time.Time] `json:"at"`
	Tags  optional.Field[[]string]   `json:"tags"`
}

func TestFieldTellsAbsentFromNullFromValue(t *testing.T) {
	t.Parallel()
	var b body
	if err := json.Unmarshal([]byte(`{"name":"Hack","count":null,"tags":[]}`), &b); err != nil {
		t.Fatal(err)
	}
	if !b.Name.Set || b.Name.Null || b.Name.Value != "Hack" {
		t.Fatalf("name %+v", b.Name)
	}
	if !b.Count.Set || !b.Count.Null || b.Count.Value != 0 {
		t.Fatalf("count %+v", b.Count)
	}
	if b.At.Set || b.At.Null || b.At.Value != nil {
		t.Fatalf("at %+v", b.At)
	}
	if !b.Tags.Set || b.Tags.Null || b.Tags.Value == nil || len(b.Tags.Value) != 0 {
		t.Fatalf("tags %+v", b.Tags)
	}
}

func TestFieldNullClearsAnEarlierValue(t *testing.T) {
	t.Parallel()
	var b body
	if err := json.Unmarshal([]byte(`{"name":"Hack","name":null}`), &b); err != nil {
		t.Fatal(err)
	}
	if !b.Name.Set || !b.Name.Null || b.Name.Value != "" {
		t.Fatalf("name %+v", b.Name)
	}
}

func TestFieldRefusesTheWrongType(t *testing.T) {
	t.Parallel()
	var b body
	if err := json.Unmarshal([]byte(`{"count":"seven"}`), &b); err == nil {
		t.Fatal("a string for an int field was accepted")
	}
}

func TestFieldOr(t *testing.T) {
	t.Parallel()
	if got := (optional.Field[string]{}).Or("kept"); got != "kept" {
		t.Fatalf("absent: %q", got)
	}
	if got := (optional.Field[string]{Set: true, Null: true}).Or("kept"); got != "" {
		t.Fatalf("null: %q", got)
	}
	if got := (optional.Field[string]{Set: true, Value: "new"}).Or("kept"); got != "new" {
		t.Fatalf("value: %q", got)
	}
}
