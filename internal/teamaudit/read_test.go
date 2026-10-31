package teamaudit_test

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
)

// readerUnderTest is a writer that also reads, with a clock: the memory
// writer and the Postgres one answer the same reads alike.
type readerUnderTest interface {
	teamaudit.Writer
	teamaudit.Reader
	SetClock(func() time.Time)
}

// auditClock is a clock a test moves.
type auditClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *auditClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *auditClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

// seeded is the six rows every reader test reads, by name.
type seeded struct {
	ids                              map[string]uuid.UUID
	actorA, actorB, userU, userV     uuid.UUID
	weblab, front, oldTeam, mob, wbx string
	t0                               time.Time
}

// seedReads writes, a minute apart: r1 member.add WEBLAB (A → U, ok), r2
// member.remove FRONT (B → U, failed), r3 team.delete of a Sub-team OLD
// under WEBLAB (A, ok), r4 a denied member.add MOBILAB (A → V) and r5
// group.roles WEBLAB (B, pending) at the same instant, r6 member.add of
// WEBLABX (B → V, ok), whose path only starts like WEBLAB's.
func seedReads(t *testing.T, w readerUnderTest) seeded {
	t.Helper()
	ctx := context.Background()
	s := seeded{
		ids:    map[string]uuid.UUID{},
		actorA: uuid.New(), actorB: uuid.New(), userU: uuid.New(), userV: uuid.New(),
		weblab: uuid.NewString(), front: uuid.NewString(), oldTeam: uuid.NewString(), mob: uuid.NewString(), wbx: uuid.NewString(),
		t0: time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC),
	}
	clock := &auditClock{}
	w.SetClock(clock.Now)
	actor := func(id uuid.UUID) teamaudit.Actor {
		return teamaudit.Actor{ID: id, Basis: teamaudit.BasisPrivileged, Client: "admin", CorrelationID: "req"}
	}
	write := func(name string, minute int, r teamaudit.Record, outcome teamaudit.Outcome, code string) {
		t.Helper()
		clock.set(s.t0.Add(time.Duration(minute) * time.Minute))
		if outcome == teamaudit.OutcomeDenied {
			if err := w.Deny(ctx, r, code); err != nil {
				t.Fatal(err)
			}
			// Deny gives no id: the newest row of the minute is it.
			rows, _, err := w.Records(ctx, teamaudit.Query{From: clock.Now(), Outcome: teamaudit.OutcomeDenied, Limit: 1})
			if err != nil || len(rows) != 1 {
				t.Fatalf("denied row: %v %v", rows, err)
			}
			s.ids[name] = rows[0].ID
			return
		}
		id, err := w.Begin(ctx, r)
		if err != nil {
			t.Fatal(err)
		}
		s.ids[name] = id
		if outcome != teamaudit.OutcomePending {
			if err := w.Finish(ctx, id, outcome, code, nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	write("r1", 0, teamaudit.Record{Actor: actor(s.actorA), Action: teamaudit.ActionMemberAdd, GroupID: s.weblab, GroupPath: "/UYELER/ARGE/WEBLAB", TargetUserID: s.userU}, teamaudit.OutcomeOK, "")
	write("r2", 1, teamaudit.Record{Actor: actor(s.actorB), Action: teamaudit.ActionMemberRemove, GroupID: s.front, GroupPath: "/UYELER/ARGE/WEBLAB/FRONT", TargetUserID: s.userU}, teamaudit.OutcomeFailed, "keycloak_400")
	write("r3", 2, teamaudit.Record{Actor: actor(s.actorA), Action: teamaudit.ActionTeamDelete, GroupID: s.oldTeam, GroupPath: "/UYELER/ARGE/WEBLAB/OLD"}, teamaudit.OutcomeOK, "")
	denied := teamaudit.Record{Actor: actor(s.actorA), Action: teamaudit.ActionMemberAdd, GroupID: s.mob, GroupPath: "/UYELER/ARGE/MOBILAB", TargetUserID: s.userV}
	denied.Actor.Basis = teamaudit.BasisNone
	write("r4", 3, denied, teamaudit.OutcomeDenied, "forbidden")
	write("r5", 3, teamaudit.Record{Actor: actor(s.actorB), Action: teamaudit.ActionGroupRoles, GroupID: s.weblab, GroupPath: "/UYELER/ARGE/WEBLAB"}, teamaudit.OutcomePending, "")
	write("r6", 4, teamaudit.Record{Actor: actor(s.actorB), Action: teamaudit.ActionMemberAdd, GroupID: s.wbx, GroupPath: "/UYELER/ARGE/WEBLABX", TargetUserID: s.userV}, teamaudit.OutcomeOK, "")
	return s
}

// order is the names in the read order: r4 and r5 share an instant, so the
// greater id comes first.
func (s seeded) order(names ...string) []string {
	out := slices.Clone(names)
	at := map[string]int{"r1": 0, "r2": 1, "r3": 2, "r4": 3, "r5": 3, "r6": 4}
	slices.SortFunc(out, func(a, b string) int {
		if at[a] != at[b] {
			return at[b] - at[a]
		}
		return strings.Compare(s.ids[b].String(), s.ids[a].String())
	})
	return out
}

func (s seeded) names(rows []teamaudit.Row) []string {
	by := map[uuid.UUID]string{}
	for name, id := range s.ids {
		by[id] = name
	}
	out := []string{}
	for _, r := range rows {
		out = append(out, by[r.ID])
	}
	return out
}

// testReader runs the filter and cursor tables against w.
func testReader(t *testing.T, w readerUnderTest) {
	ctx := context.Background()
	s := seedReads(t, w)
	all := []string{"r1", "r2", "r3", "r4", "r5", "r6"}
	for _, tc := range []struct {
		name string
		q    teamaudit.Query
		want []string
	}{
		{"everything", teamaudit.Query{}, all},
		{"one Group", teamaudit.Query{GroupIDs: []string{s.weblab}}, []string{"r1", "r5"}},
		{"a subtree: its Groups now and the paths under it, not WEBLABX", teamaudit.Query{GroupIDs: []string{s.weblab, s.front}, GroupPath: "/UYELER/ARGE/WEBLAB"}, []string{"r1", "r2", "r3", "r5"}},
		{"a gone Group's subtree by path alone", teamaudit.Query{GroupIDs: []string{uuid.NewString()}, GroupPath: "/UYELER/ARGE/WEBLAB/OLD"}, []string{"r3"}},
		{"an actor", teamaudit.Query{ActorID: s.actorA}, []string{"r1", "r3", "r4"}},
		{"a target", teamaudit.Query{TargetUserID: s.userU}, []string{"r1", "r2"}},
		{"several actions", teamaudit.Query{Actions: []teamaudit.Action{teamaudit.ActionMemberAdd, teamaudit.ActionMemberRemove}}, []string{"r1", "r2", "r4", "r6"}},
		{"an outcome", teamaudit.Query{Outcome: teamaudit.OutcomeFailed}, []string{"r2"}},
		{"pending", teamaudit.Query{Outcome: teamaudit.OutcomePending}, []string{"r5"}},
		{"from inclusive, to exclusive", teamaudit.Query{From: s.t0.Add(time.Minute), To: s.t0.Add(3 * time.Minute)}, []string{"r2", "r3"}},
		{"filters together", teamaudit.Query{ActorID: s.actorA, Actions: []teamaudit.Action{teamaudit.ActionMemberAdd}}, []string{"r1", "r4"}},
		{"nothing matches", teamaudit.Query{ActorID: uuid.New()}, []string{}},
	} {
		tc.q.Limit = teamaudit.MaxPage
		rows, next, err := w.Records(ctx, tc.q)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got, want := s.names(rows), s.order(tc.want...); !slices.Equal(got, want) || !next.IsZero() {
			t.Errorf("%s: %v (next %v), want %v", tc.name, got, next, want)
		}
	}

	// Pages of two walk the whole order once, the shared instant of r4
	// and r5 split over two pages, and the last says no next.
	for _, q := range []teamaudit.Query{{}, {Actions: []teamaudit.Action{teamaudit.ActionMemberAdd, teamaudit.ActionGroupRoles}}} {
		want := all
		if len(q.Actions) > 0 {
			want = []string{"r1", "r4", "r5", "r6"}
		}
		walked := []string{}
		for pages := 0; ; pages++ {
			if pages > len(all) {
				t.Fatal("the pages do not end")
			}
			q.Limit = 2
			rows, next, err := w.Records(ctx, q)
			if err != nil {
				t.Fatal(err)
			}
			walked = append(walked, s.names(rows)...)
			if next.IsZero() {
				break
			}
			if len(rows) != 2 {
				t.Fatalf("a page of %d with a next", len(rows))
			}
			// The cursor travels as its token.
			if q.After, err = teamaudit.ParseCursor(next.String()); err != nil {
				t.Fatal(err)
			}
		}
		if !slices.Equal(walked, s.order(want...)) {
			t.Errorf("%v: walked %v, want %v", q.Actions, walked, s.order(want...))
		}
	}

	// A row comes back as it was written.
	rows, _, err := w.Records(ctx, teamaudit.Query{GroupIDs: []string{s.front}, Limit: 1})
	if err != nil || len(rows) != 1 {
		t.Fatalf("FRONT: %v %v", rows, err)
	}
	r := rows[0]
	if r.Actor.ID != s.actorB || r.Actor.Basis != teamaudit.BasisPrivileged || r.Actor.Client != "admin" || r.Actor.CorrelationID != "req" ||
		r.GroupID != s.front || r.GroupPath != "/UYELER/ARGE/WEBLAB/FRONT" || r.TargetUserID != s.userU ||
		r.Outcome != teamaudit.OutcomeFailed || r.ErrorCode != "keycloak_400" || !r.OccurredAt.Equal(s.t0.Add(time.Minute)) {
		t.Fatalf("FRONT row %+v", r)
	}

	if _, _, err := w.Records(ctx, teamaudit.Query{Limit: teamaudit.MaxPage + 1}); err == nil {
		t.Fatal("a page over the limit was read")
	}
}

func TestMemoryRecords(t *testing.T) {
	t.Parallel()
	testReader(t, teamaudit.NewMemory())
}

func TestPostgresRecords(t *testing.T) {
	testReader(t, teamaudit.NewPostgres(migrated(t)))
}

func TestRecordsUnavailable(t *testing.T) {
	t.Parallel()
	m := teamaudit.NewMemory()
	m.SetUnavailable(true)
	if _, _, err := m.Records(context.Background(), teamaudit.Query{Limit: 1}); err == nil {
		t.Fatal("read from a database that is down")
	}
}

// A cursor is opaque but round-trips; anything else is refused.
func TestCursorToken(t *testing.T) {
	t.Parallel()
	c := teamaudit.Cursor{OccurredAt: time.Date(2026, 10, 31, 9, 0, 0, 123456000, time.UTC), ID: uuid.New()}
	back, err := teamaudit.ParseCursor(c.String())
	if err != nil || !back.OccurredAt.Equal(c.OccurredAt) || back.ID != c.ID {
		t.Fatalf("round trip %v %v", back, err)
	}
	if (teamaudit.Cursor{}).String() != "" {
		t.Fatal("the zero cursor has a token")
	}
	for _, bad := range []string{"", "!!", "bm90LWEtY3Vyc29y", "MTIzLm5vdC1hLXV1aWQ", "eC4wMDAwMDAwMC0wMDAwLTAwMDAtMDAwMC0wMDAwMDAwMDAwMDA"} {
		if _, err := teamaudit.ParseCursor(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}
