package teamaudit

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func validRecord() Record {
	return Record{
		Actor:        Actor{ID: uuid.New(), Basis: BasisPrivileged, Client: "admin-panel", CorrelationID: "req-1"},
		Action:       ActionMemberAdd,
		GroupID:      uuid.NewString(),
		GroupPath:    "/UYELER/ARGE/WEBLAB",
		TargetUserID: uuid.New(),
	}
}

// A row the table would refuse is refused before it is written, with a
// code: the write then stops (503), it does not go on unrecorded.
func TestRecordNormalizeRefusesWhatTheTableRefuses(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		change func(*Record)
		code   string
	}{
		"no actor":                       {func(r *Record) { r.Actor.ID = uuid.Nil }, "no_actor"},
		"no basis":                       {func(r *Record) { r.Actor.Basis = "" }, "invalid_basis"},
		"privileged through a team":      {func(r *Record) { r.Actor.ViaGroupID = uuid.NewString() }, "invalid_basis"},
		"leader without a team":          {func(r *Record) { r.Actor.Basis = BasisLeader }, "invalid_basis"},
		"no basis through a team":        {func(r *Record) { r.Actor.Basis, r.Actor.ViaGroupID = BasisNone, uuid.NewString() }, "invalid_basis"},
		"unknown action":                 {func(r *Record) { r.Action = "member.promote" }, "invalid_action"},
		"no group":                       {func(r *Record) { r.GroupID = "" }, "no_group"},
		"group id not a uuid":            {func(r *Record) { r.GroupID = "g-uyeler" }, "invalid_group"},
		"relative path":                  {func(r *Record) { r.GroupPath = "UYELER" }, "invalid_path"},
		"path too long":                  {func(r *Record) { r.GroupPath = "/" + strings.Repeat("a", MaxPath) }, "invalid_path"},
		"member action without a person": {func(r *Record) { r.TargetUserID = uuid.Nil }, "invalid_target"},
		"group action with a person":     {func(r *Record) { r.Action = ActionGroupRoles }, "invalid_target"},
	} {
		r := validRecord()
		c.change(&r)
		if _, got := r.normalize(); got != c.code {
			t.Errorf("%s: %q, want %q", name, got, c.code)
		}
	}
	r := validRecord()
	r.Actor.Basis, r.Actor.ViaGroupID = BasisLeader, uuid.NewString()
	if _, bad := r.normalize(); bad != "" {
		t.Fatalf("leader: %s", bad)
	}
	create := validRecord()
	create.Action, create.GroupID, create.TargetUserID = ActionTeamCreate, "", uuid.Nil
	if _, bad := create.normalize(); bad != "" {
		t.Fatalf("create without its id: %s", bad)
	}
}

// Texts are cut to their limits, a caller's request id that is not a plain
// token is not kept, and only the recorded attributes keep their values.
func TestRecordNormalizeCutsAndKeepsOnlyWhatARowHolds(t *testing.T) {
	t.Parallel()
	r := validRecord()
	r.Actor.CorrelationID = "ada@example.com <script>"
	r.Actor.Client = strings.Repeat("c", 300)
	r.After = &State{
		Code: strings.Repeat("K", 300),
		Attributes: map[string]string{
			"description_tr": strings.Repeat("ç", 400),
			"contact_email":  "ada@example.com",
		},
		OtherAttributes: []string{"b", "a", "b", strings.Repeat("x", 100)},
		Roles:           []string{"core:events:manage", "core:events:manage", "cms:access"},
	}
	got, bad := r.normalize()
	if bad != "" {
		t.Fatal(bad)
	}
	if got.Actor.CorrelationID != "" || len([]rune(got.Actor.Client)) != MaxIdentifier {
		t.Fatalf("actor %+v", got.Actor)
	}
	if len([]rune(got.After.Code)) != MaxIdentifier || len([]rune(got.After.Attributes["description_tr"])) != MaxText {
		t.Fatalf("not cut: %d %d", len(got.After.Code), len(got.After.Attributes["description_tr"]))
	}
	if _, kept := got.After.Attributes["contact_email"]; kept {
		t.Fatal("an attribute outside the recorded ones kept its value")
	}
	if !reflect.DeepEqual(got.After.OtherAttributes, []string{"a", "b", strings.Repeat("x", MaxAttributeName)}) {
		t.Fatalf("others %v", got.After.OtherAttributes)
	}
	if !reflect.DeepEqual(got.After.Roles, []string{"cms:access", "core:events:manage"}) {
		t.Fatalf("roles %v", got.After.Roles)
	}
	if r.After.Code == got.After.Code {
		t.Fatal("normalize changed the caller's state")
	}
}

func TestAttributeChangeKeepsValuesOfRecordedAttributesOnly(t *testing.T) {
	t.Parallel()
	before, after, others := AttributeChange(
		map[string]string{"display_name_tr": "Web", "public_listing": "true", "secret_note": "x", "same": "1"},
		map[string]string{"display_name_tr": "Web Lab", "team_door_scan": "true", "secret_note": "y", "same": "1"},
	)
	if !reflect.DeepEqual(before, map[string]string{"display_name_tr": "Web", "public_listing": "true"}) ||
		!reflect.DeepEqual(after, map[string]string{"display_name_tr": "Web Lab", "team_door_scan": "true"}) ||
		!reflect.DeepEqual(others, []string{"secret_note"}) {
		t.Fatalf("before %v after %v others %v", before, after, others)
	}
	before, after, others = AttributeChange(map[string]string{"a": "1"}, map[string]string{"a": "1"})
	if before != nil || after != nil || others != nil {
		t.Fatal("no change recorded a change")
	}
}

func TestMemoryWriter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	m := NewMemory()
	id, err := m.Begin(ctx, validRecord())
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Finish(ctx, id, OutcomeOK, "", nil); err != nil {
		t.Fatal(err)
	}
	if err := m.Finish(ctx, id, OutcomeFailed, "not_found", nil); !errors.Is(err, ErrNotPending) {
		t.Fatalf("finished twice: %v", err)
	}
	if err := m.Finish(ctx, id, OutcomeFailed, "", nil); err == nil {
		t.Fatal("failed without a code")
	}
	create := validRecord()
	create.Action, create.GroupID, create.TargetUserID = ActionTeamCreate, "", uuid.Nil
	createID, err := m.Begin(ctx, create)
	if err != nil {
		t.Fatal(err)
	}
	newID := uuid.NewString()
	if err := m.Finish(ctx, createID, OutcomeOK, "", &State{ID: newID, Path: "/UYELER/ARGE/WEBLAB"}); err != nil {
		t.Fatal(err)
	}
	if err := m.Deny(ctx, validRecord(), "forbidden"); err != nil {
		t.Fatal(err)
	}
	rows := m.Rows()
	if len(rows) != 3 || rows[0].Outcome != OutcomeOK || rows[1].GroupID != newID || rows[2].Outcome != OutcomeDenied || rows[2].ErrorCode != "forbidden" {
		t.Fatalf("rows %+v", rows)
	}
	m.SetUnavailable(true)
	if _, err := m.Begin(ctx, validRecord()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("down: %v", err)
	}
	if err := m.Deny(ctx, validRecord(), "forbidden"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("down deny: %v", err)
	}
}

func TestActorTravelsInTheContext(t *testing.T) {
	t.Parallel()
	if _, ok := ActorFrom(context.Background()); ok {
		t.Fatal("actor from nowhere")
	}
	a := Actor{ID: uuid.New(), Basis: BasisPrivileged}
	got, ok := ActorFrom(WithActor(context.Background(), a))
	if !ok || got != a {
		t.Fatalf("actor %+v", got)
	}
}

// A caller with no basis for the change appears only in a denied row: a
// change that is made always has one (ticket 04).
func TestBasisNoneOnlyDenied(t *testing.T) {
	t.Parallel()
	m := NewMemory()
	r := validRecord()
	r.Actor.Basis = BasisNone
	if _, err := m.Begin(context.Background(), r); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("begun without a basis: %v", err)
	}
	if err := m.Deny(context.Background(), r, "forbidden"); err != nil {
		t.Fatalf("denied without a basis: %v", err)
	}
	if rows := m.Rows(); len(rows) != 1 || rows[0].Outcome != OutcomeDenied || rows[0].Actor.Basis != BasisNone {
		t.Fatalf("rows %+v", rows)
	}
}
