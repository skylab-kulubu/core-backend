package teamaudit

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func appRecord(action Action, client, role string) Record {
	return Record{
		Actor:  Actor{ID: uuid.New(), Basis: BasisPrivileged, Client: "admin"},
		Action: action, GroupID: uuid.NewString(), GroupPath: "/UYELER/ARGE/WEBLAB",
		After: &State{Client: client, Role: role},
	}
}

// An app row names one target, a Group by id and path or a person without a
// path, and its app role; nothing else of the change is kept.
func TestAppRecordNormalize(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		change func(*Record)
		code   string
	}{
		"both targets":            {func(r *Record) { r.TargetUserID = uuid.New() }, "invalid_target"},
		"no target":               {func(r *Record) { r.GroupID, r.GroupPath = "", "" }, "invalid_target"},
		"a person with a path":    {func(r *Record) { r.GroupID, r.TargetUserID = "", uuid.New() }, "invalid_path"},
		"a Group without path":    {func(r *Record) { r.GroupPath = "" }, "invalid_path"},
		"no role":                 {func(r *Record) { r.After.Role = "" }, "no_app_role"},
		"no client":               {func(r *Record) { r.After.Client = "" }, "no_app_role"},
		"no after":                {func(r *Record) { r.After = nil }, "no_app_role"},
		"group id not a uuid":     {func(r *Record) { r.GroupID = "weblab" }, "invalid_group"},
		"a person on group.roles": {func(r *Record) { r.Action = ActionGroupRoles; r.TargetUserID = uuid.New() }, "invalid_target"},
	} {
		r := appRecord(ActionAppAssign, "forms", "skyforms:access")
		c.change(&r)
		if _, got := r.normalize(); got != c.code {
			t.Errorf("%s: %q, want %q", name, got, c.code)
		}
	}
	person := appRecord(ActionAppUnassign, "forms", "skyforms:access")
	person.GroupID, person.GroupPath, person.TargetUserID = "", "", uuid.New()
	person.After.Role = strings.Repeat("ş", MaxIdentifier+20)
	got, bad := person.normalize()
	if bad != "" {
		t.Fatalf("person: %s", bad)
	}
	if n := len([]rune(got.After.Role)); n != MaxIdentifier {
		t.Errorf("role of %d runes", n)
	}
	if _, bad := appRecord(ActionAppAssign, "forms", "skyforms:access").normalize(); bad != "" {
		t.Errorf("group: %s", bad)
	}
}

// AppRows are the client's app rows newest first, every outcome; the latest
// rows are one per target and role, the newest that ended ok.
func TestMemoryAppRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	m := NewMemory()
	group := appRecord(ActionAppAssign, "forms", "skyforms:access")
	first, err := m.Begin(ctx, group)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Finish(ctx, first, OutcomeOK, "", nil); err != nil {
		t.Fatal(err)
	}
	again := group
	again.Action = ActionAppUnassign
	second, err := m.Begin(ctx, again)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Finish(ctx, second, OutcomeOK, "", nil); err != nil {
		t.Fatal(err)
	}
	failed := group
	third, err := m.Begin(ctx, failed)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Finish(ctx, third, OutcomeFailed, "keycloak_400", nil); err != nil {
		t.Fatal(err)
	}
	if err := m.Deny(ctx, appRecord(ActionAppAssign, "forms", "skyforms:*"), "role_placement"); err != nil {
		t.Fatal(err)
	}
	other := appRecord(ActionAppAssign, "skymail", "skymail:access")
	if _, err := m.Begin(ctx, other); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Begin(ctx, validRecord()); err != nil {
		t.Fatal(err)
	}

	rows, err := m.AppRows(ctx, "forms", 100)
	if err != nil || len(rows) != 4 || rows[0].Outcome != OutcomeDenied || rows[3].ID != first {
		t.Fatalf("rows %+v %v", rows, err)
	}
	if rows, _ := m.AppRows(ctx, "forms", 2); len(rows) != 2 {
		t.Errorf("limit: %d", len(rows))
	}
	latest, err := m.LatestAppRows(ctx, "forms")
	if err != nil || len(latest) != 1 || latest[0].ID != second || latest[0].Action != ActionAppUnassign {
		t.Fatalf("latest %+v %v", latest, err)
	}
	m.SetUnavailable(true)
	if _, err := m.AppRows(ctx, "forms", 1); err == nil {
		t.Error("down: read")
	}
}
