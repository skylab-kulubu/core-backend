package competition_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/competition"
	"github.com/skylab-kulubu/core-backend/internal/competition/scoring"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/lifecycle"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

var (
	agcLeader = authz.Principal{ID: "11111111-1111-1111-1111-111111111111", Groups: []string{"/UYELER/ARGE/ALGOLAB/AGC/LIDERLER"}}
	agcMember = authz.Principal{ID: "22222222-2222-2222-2222-222222222222", Groups: []string{"/UYELER/ARGE/ALGOLAB/AGC"}}
	ykMember  = authz.Principal{ID: "33333333-3333-3333-3333-333333333333", Groups: []string{"/UYELER/YK"}}
)

// userStore is what the fixtures write users with, in memory or in
// PostgreSQL.
type userStore interface {
	competition.UserReader
	Upsert(ctx context.Context, u user.User) (user.User, bool, error)
	RequestDeletion(ctx context.Context, id uuid.UUID, requestedBy *uuid.UUID) (user.DeletionRequest, error)
}

type fixture struct {
	svc     competition.Service
	events  event.Store
	tickets ticket.Store
	users   userStore
	event   event.Event
}

// newFixture is an AGC Event, not active: adding Entrants no longer
// depends on the Event being active (spec §7).
func newFixture(t *testing.T, store func(ticket.Store, userStore) competition.Store) fixture {
	t.Helper()
	return newFixtureWith(t, event.NewMemoryStore(), ticket.NewMemoryStore(), user.NewMemoryStore(), store)
}

func newFixtureWith(t *testing.T, events event.Store, tickets ticket.Store, users userStore, store func(ticket.Store, userStore) competition.Store) fixture {
	t.Helper()
	ev, err := events.Create(t.Context(), event.Event{Name: "AGC Hafta 3", Location: "YTÜ", OwnerTeam: "AGC", Active: false})
	if err != nil {
		t.Fatal(err)
	}
	return fixture{
		svc:    competition.NewService(store(tickets, users), events, authz.NewAuthorizer(authz.DefaultPolicy())),
		events: events, tickets: tickets, users: users, event: ev,
	}
}

func memoryStore(tickets ticket.Store, users userStore) competition.Store {
	return competition.NewMemoryStore(tickets, users)
}

func (f fixture) registered(t *testing.T, first, last string) ticket.Ticket {
	t.Helper()
	u, _, err := f.users.Upsert(t.Context(), user.User{ID: uuid.New(), Email: uuid.NewString() + "@example.test", FirstName: first, LastName: last})
	if err != nil {
		t.Fatal(err)
	}
	tk, err := f.tickets.Create(t.Context(), ticket.Ticket{EventID: f.event.ID, TicketType: ticket.Registered, OwnerID: &u.ID})
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

func (f fixture) guest(t *testing.T, first, last string) ticket.Ticket {
	t.Helper()
	tk, err := f.tickets.Create(t.Context(), ticket.Ticket{
		EventID: f.event.ID, TicketType: ticket.Guest, GuestFirstName: first, GuestLastName: last, GuestEmail: uuid.NewString() + "@example.test",
	})
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

func (f fixture) create(t *testing.T) competition.Competition {
	t.Helper()
	c, err := f.svc.Create(t.Context(), agcLeader, f.event.ID, competition.CreateInput{Name: "AGC Hafta 3"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func pts(p float64) scoring.Points { return scoring.Points(p * scoring.Scale) }

func TestServiceCreatesACompetitionWithOneManualStage(t *testing.T) {
	t.Parallel()
	f := newFixture(t, memoryStore)
	c := f.create(t)
	if c.EventID != f.event.ID || c.OwnerTeam != "AGC" || c.Status != competition.StatusDraft || c.EntrantMode != competition.ModeIndividual {
		t.Fatalf("created %+v", c)
	}
	if len(c.Stages) != 1 || c.Stages[0].Strategy != "manual" || c.Stages[0].Position != 1 {
		t.Fatalf("stages %+v", c.Stages)
	}
	if c.CreatedBy == nil || c.CreatedBy.String() != agcLeader.ID {
		t.Fatalf("created by %v", c.CreatedBy)
	}
	list, err := f.svc.ListByEvent(t.Context(), agcLeader, f.event.ID, lifecycle.CurrentOnly)
	if err != nil || len(list) != 1 || list[0].ID != c.ID {
		t.Fatalf("list %+v %v", list, err)
	}
}

func TestServiceRefusesACompetitionWithoutItsEvent(t *testing.T) {
	t.Parallel()
	f := newFixture(t, memoryStore)
	for _, id := range []uuid.UUID{uuid.Nil, uuid.New()} {
		if _, err := f.svc.Create(t.Context(), ykMember, id, competition.CreateInput{Name: "x"}); !errors.Is(err, competition.ErrNotFound) {
			t.Errorf("event %s: %v, want ErrNotFound", id, err)
		}
	}
	// An archived Event takes no new Competition, and hides its own.
	c := f.create(t)
	if err := f.events.Archive(t.Context(), f.event.ID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Create(t.Context(), ykMember, f.event.ID, competition.CreateInput{Name: "x"}); !errors.Is(err, competition.ErrNotFound) {
		t.Errorf("archived event: %v", err)
	}
	if _, err := f.svc.Get(t.Context(), ykMember, c.ID); !errors.Is(err, competition.ErrNotFound) {
		t.Errorf("competition of an archived event: %v", err)
	}
}

func TestServiceValidatesTheCompetition(t *testing.T) {
	t.Parallel()
	f := newFixture(t, memoryStore)
	long := make([]byte, 201)
	for i := range long {
		long[i] = 'a'
	}
	for name, in := range map[string]competition.CreateInput{
		"empty name":   {Name: "  "},
		"long name":    {Name: string(long)},
		"teams (11)":   {Name: "x", EntrantMode: competition.ModeTeam},
		"unknown mode": {Name: "x", EntrantMode: "pairs"},
	} {
		if _, err := f.svc.Create(t.Context(), agcLeader, f.event.ID, in); !errors.Is(err, competition.ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
	c := f.create(t)
	name, desc := " AGC 3. hafta ", "Haftalık"
	got, err := f.svc.Update(t.Context(), agcLeader, c.ID, competition.UpdateInput{Name: &name, Description: &desc})
	if err != nil || got.Name != "AGC 3. hafta" || got.Description != "Haftalık" {
		t.Fatalf("update %+v %v", got, err)
	}
	empty := ""
	if _, err := f.svc.Update(t.Context(), agcLeader, c.ID, competition.UpdateInput{Name: &empty}); !errors.Is(err, competition.ErrInvalid) {
		t.Errorf("empty name update: %v", err)
	}
}

func TestServiceAuthorizesOrganizersOnly(t *testing.T) {
	t.Parallel()
	f := newFixture(t, memoryStore)
	weblabLeader := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
	for name, p := range map[string]authz.Principal{"plain member": agcMember, "other team's leader": weblabLeader, "anonymous": {}} {
		if _, err := f.svc.Create(t.Context(), p, f.event.ID, competition.CreateInput{Name: "x"}); !errors.Is(err, competition.ErrForbidden) {
			t.Errorf("%s create: %v", name, err)
		}
	}
	c := f.create(t)
	tk := f.guest(t, "Ada", "Lovelace")
	for name, p := range map[string]authz.Principal{"plain member": agcMember, "other team's leader": weblabLeader} {
		if _, err := f.svc.Get(t.Context(), p, c.ID); !errors.Is(err, competition.ErrForbidden) {
			t.Errorf("%s get: %v", name, err)
		}
		if _, err := f.svc.AddEntrants(t.Context(), p, c.ID, []uuid.UUID{tk.ID}); !errors.Is(err, competition.ErrForbidden) {
			t.Errorf("%s add entrants: %v", name, err)
		}
		if _, err := f.svc.Leaderboard(t.Context(), p, c.ID); !errors.Is(err, competition.ErrForbidden) {
			t.Errorf("%s leaderboard: %v", name, err)
		}
		if _, err := f.svc.ListByEvent(t.Context(), p, f.event.ID, lifecycle.CurrentOnly); !errors.Is(err, competition.ErrForbidden) {
			t.Errorf("%s list: %v", name, err)
		}
	}
	if _, err := f.svc.Get(t.Context(), ykMember, c.ID); err != nil {
		t.Errorf("YK get: %v", err)
	}
	holder := authz.Principal{ID: uuid.NewString(), Roles: []string{authz.RoleCompetitionManage}}
	roles := competition.NewService(competition.NewMemoryStore(f.tickets, f.users), f.events, authz.NewAuthorizer(func() authz.Policy {
		p := authz.DefaultPolicy()
		p.RoleMode = authz.RoleModeRoles
		return p
	}()))
	if _, err := roles.Create(t.Context(), holder, f.event.ID, competition.CreateInput{Name: "x"}); err != nil {
		t.Errorf("competition:manage holder in the roles mode: %v", err)
	}
}

// An Event without an Owner team: only the Privileged manage its
// Competitions (ADR-0018).
func TestServiceEventWithoutOwnerTeamIsPrivilegedOnly(t *testing.T) {
	t.Parallel()
	f := newFixture(t, memoryStore)
	club, err := f.events.Create(t.Context(), event.Event{Name: "Kulüp yarışması", Location: "YTÜ"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Create(t.Context(), agcLeader, club.ID, competition.CreateInput{Name: "x"}); !errors.Is(err, competition.ErrForbidden) {
		t.Errorf("leader: %v", err)
	}
	if _, err := f.svc.Create(t.Context(), ykMember, club.ID, competition.CreateInput{Name: "x"}); err != nil {
		t.Errorf("YK: %v", err)
	}
}

func TestServiceAddsMembersAndGuestsOnce(t *testing.T) {
	t.Parallel()
	f := newFixture(t, memoryStore)
	c := f.create(t)
	ada := f.registered(t, "Ada", "Lovelace")
	grace := f.guest(t, "Grace", "Hopper")
	added, err := f.svc.AddEntrants(t.Context(), agcLeader, c.ID, []uuid.UUID{ada.ID, grace.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(added) != 2 || added[0].DisplayName != "Ada Lovelace" || added[1].DisplayName != "Grace Hopper" {
		t.Fatalf("added %+v", added)
	}
	if added[1].Members[0].TicketType != ticket.Guest || added[1].Kind != competition.KindIndividual || added[1].Status != competition.EntrantActive {
		t.Fatalf("guest entrant %+v", added[1])
	}
	// The same Ticket again, alone or with a new one: nothing is written.
	linus := f.guest(t, "Linus", "Torvalds")
	if _, err := f.svc.AddEntrants(t.Context(), agcLeader, c.ID, []uuid.UUID{linus.ID, ada.ID}); !errors.Is(err, competition.ErrTicketTaken) {
		t.Fatalf("second time: %v, want ErrTicketTaken", err)
	}
	list, err := f.svc.Entrants(t.Context(), agcLeader, c.ID)
	if err != nil || len(list) != 2 {
		t.Fatalf("entrants %+v %v", list, err)
	}
	// Duplicates in one request, no Ticket, a Ticket of another Event.
	other, err := f.events.Create(t.Context(), event.Event{Name: "Başka", Location: "YTÜ", OwnerTeam: "AGC"})
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := f.tickets.Create(t.Context(), ticket.Ticket{EventID: other.ID, TicketType: ticket.Guest, GuestFirstName: "X", GuestEmail: "x@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	for name, ids := range map[string][]uuid.UUID{
		"duplicate":       {linus.ID, linus.ID},
		"none":            {},
		"nil":             {uuid.Nil},
		"unknown":         {uuid.New()},
		"another event's": {foreign.ID},
	} {
		if _, err := f.svc.AddEntrants(t.Context(), agcLeader, c.ID, ids); !errors.Is(err, competition.ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
}

func TestServiceAddsEntrantsWhileDraftOpenOrRunning(t *testing.T) {
	t.Parallel()
	f := newFixture(t, memoryStore)
	c := f.create(t)
	steps := []struct {
		do     func() (competition.Competition, error)
		status competition.Status
		adds   bool
	}{
		{func() (competition.Competition, error) { return f.svc.Open(t.Context(), agcLeader, c.ID) }, competition.StatusOpen, true},
		{func() (competition.Competition, error) { return f.svc.Start(t.Context(), agcLeader, c.ID) }, competition.StatusRunning, true},
		{func() (competition.Competition, error) { return f.svc.Close(t.Context(), agcLeader, c.ID) }, competition.StatusClosed, false},
	}
	if _, err := f.svc.AddEntrants(t.Context(), agcLeader, c.ID, []uuid.UUID{f.guest(t, "D", "Raft").ID}); err != nil {
		t.Fatalf("draft: %v", err)
	}
	for _, step := range steps {
		got, err := step.do()
		if err != nil || got.Status != step.status {
			t.Fatalf("to %s: %+v %v", step.status, got, err)
		}
		_, err = f.svc.AddEntrants(t.Context(), agcLeader, c.ID, []uuid.UUID{f.guest(t, "G", string(step.status)).ID})
		if step.adds && err != nil {
			t.Errorf("%s: %v", step.status, err)
		}
		if !step.adds && !errors.Is(err, competition.ErrStatus) {
			t.Errorf("%s: %v, want ErrStatus", step.status, err)
		}
	}
}

func TestServiceStatusTransitions(t *testing.T) {
	t.Parallel()
	f := newFixture(t, memoryStore)
	c := f.create(t)
	if _, err := f.svc.Close(t.Context(), agcLeader, c.ID); err != nil {
		t.Fatalf("draft → closed: %v", err)
	}
	if _, err := f.svc.Open(t.Context(), agcLeader, c.ID); !errors.Is(err, competition.ErrStatus) {
		t.Fatalf("closed → open: %v", err)
	}
	got, err := f.svc.Reopen(t.Context(), agcLeader, c.ID)
	if err != nil || got.Status != competition.StatusRunning {
		t.Fatalf("closed → running: %+v %v", got, err)
	}
	if _, err := f.svc.Cancel(t.Context(), agcLeader, c.ID, "  "); !errors.Is(err, competition.ErrReasonRequired) {
		t.Fatalf("cancel without reason: %v", err)
	}
	got, err = f.svc.Cancel(t.Context(), agcLeader, c.ID, "Etkinlik ertelendi")
	if err != nil || got.Status != competition.StatusCancelled || got.CancelReason != "Etkinlik ertelendi" {
		t.Fatalf("cancel: %+v %v", got, err)
	}
	for name, do := range map[string]func() (competition.Competition, error){
		"open":   func() (competition.Competition, error) { return f.svc.Open(t.Context(), agcLeader, c.ID) },
		"start":  func() (competition.Competition, error) { return f.svc.Start(t.Context(), agcLeader, c.ID) },
		"close":  func() (competition.Competition, error) { return f.svc.Close(t.Context(), agcLeader, c.ID) },
		"reopen": func() (competition.Competition, error) { return f.svc.Reopen(t.Context(), agcLeader, c.ID) },
		"cancel": func() (competition.Competition, error) {
			return f.svc.Cancel(t.Context(), agcLeader, c.ID, "again")
		},
	} {
		if _, err := do(); !errors.Is(err, competition.ErrStatus) {
			t.Errorf("cancelled → %s: %v", name, err)
		}
	}
	name := "x"
	if _, err := f.svc.Update(t.Context(), agcLeader, c.ID, competition.UpdateInput{Name: &name}); !errors.Is(err, competition.ErrStatus) {
		t.Errorf("update cancelled: %v", err)
	}
}

func TestServiceArchiveHidesAndRestoreBrings(t *testing.T) {
	t.Parallel()
	f := newFixture(t, memoryStore)
	c := f.create(t)
	if err := f.svc.Archive(t.Context(), agcMember, c.ID); !errors.Is(err, competition.ErrForbidden) {
		t.Fatalf("member archive: %v", err)
	}
	if err := f.svc.Archive(t.Context(), agcLeader, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Get(t.Context(), agcLeader, c.ID); !errors.Is(err, competition.ErrNotFound) {
		t.Fatalf("archived get: %v", err)
	}
	if list, err := f.svc.ListByEvent(t.Context(), agcLeader, f.event.ID, lifecycle.CurrentOnly); err != nil || len(list) != 0 {
		t.Fatalf("current list %+v %v", list, err)
	}
	if list, err := f.svc.ListByEvent(t.Context(), agcLeader, f.event.ID, lifecycle.InactiveOnly); err != nil || len(list) != 1 || list[0].ArchivedAt == nil {
		t.Fatalf("archived list %+v %v", list, err)
	}
	got, err := f.svc.Restore(t.Context(), agcLeader, c.ID)
	if err != nil || got.ArchivedAt != nil {
		t.Fatalf("restore %+v %v", got, err)
	}
}

// Scores: AGC's 30/50/60, a correction revokes with its reason and writes
// a new row, a revoked row never counts, and a closed Competition still
// takes scores (results are mostly typed in after the Event).
func TestServiceManualScoresAndLeaderboard(t *testing.T) {
	t.Parallel()
	runManualScores(t, newFixture(t, memoryStore))
}

func runManualScores(t *testing.T, f fixture) {
	t.Helper()
	ctx := t.Context()
	c := f.create(t)
	tickets := []ticket.Ticket{
		f.registered(t, "Ada", "Lovelace"), f.guest(t, "Grace", "Hopper"), f.guest(t, "Alan", "Turing"),
		f.registered(t, "Edsger", "Dijkstra"), f.guest(t, "Unscored", "Person"),
	}
	ids := make([]uuid.UUID, len(tickets))
	for i, tk := range tickets {
		ids[i] = tk.ID
	}
	entrants, err := f.svc.AddEntrants(ctx, agcLeader, c.ID, ids)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Close(ctx, agcLeader, c.ID); err != nil {
		t.Fatal(err)
	}
	set := func(i int, p scoring.Points, reason string) competition.Entry {
		t.Helper()
		e, err := f.svc.SetScore(ctx, agcLeader, c.ID, competition.ScoreInput{EntrantID: entrants[i].ID, Points: p, Reason: reason})
		if err != nil {
			t.Fatalf("set %d: %v", i, err)
		}
		return e
	}
	set(0, pts(30), "")
	set(1, pts(60), "")
	set(2, pts(50), "")
	first := set(3, pts(30), "")
	// A correction needs its reason; then it revokes the old row.
	if _, err := f.svc.SetScore(ctx, agcLeader, c.ID, competition.ScoreInput{EntrantID: entrants[3].ID, Points: pts(60)}); !errors.Is(err, competition.ErrReasonRequired) {
		t.Fatalf("correction without reason: %v", err)
	}
	second := set(3, pts(60.125), "Bonus soruyu da çözdü")
	if second.ReplacesID == nil || *second.ReplacesID != first.ID {
		t.Fatalf("correction %+v", second)
	}
	// The first score to Ada is revoked outright: she no longer counts.
	ada := latest(t, f, c.ID, entrants[0].ID)
	if _, err := f.svc.Revoke(ctx, agcLeader, c.ID, ada.ID, ""); !errors.Is(err, competition.ErrReasonRequired) {
		t.Fatalf("revoke without reason: %v", err)
	}
	if _, err := f.svc.Revoke(ctx, agcLeader, c.ID, ada.ID, "Yanlış kişiye girildi"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Revoke(ctx, agcLeader, c.ID, ada.ID, "again"); !errors.Is(err, competition.ErrConflict) {
		t.Fatalf("revoke twice: %v", err)
	}

	board, err := f.svc.Leaderboard(ctx, agcLeader, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		rank  int
		name  string
		score scoring.Points
	}
	want := []row{{1, "Edsger Dijkstra", pts(60.125)}, {2, "Grace Hopper", pts(60)}, {3, "Alan Turing", pts(50)}}
	if len(board.Rows) != 5 || board.Strategy != "manual" || board.StageID != c.Stages[0].ID {
		t.Fatalf("board %+v", board)
	}
	for i, w := range want {
		r := board.Rows[i]
		if r.Rank == nil || *r.Rank != w.rank || r.DisplayName != w.name || r.Score == nil || *r.Score != w.score {
			t.Errorf("row %d = %+v, want %+v", i, r, w)
		}
	}
	// Ada (revoked) and the unscored guest come last, without rank or score.
	for _, r := range board.Rows[3:] {
		if r.Rank != nil || r.Score != nil {
			t.Errorf("unscored row %+v", r)
		}
	}
	// The ledger keeps every row: 5 written, 2 revoked.
	entries, err := f.svc.Entries(ctx, agcLeader, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	revoked := 0
	for _, e := range entries {
		if e.RevokedAt != nil {
			revoked++
			if e.RevokeReason == "" || e.RevokedBy == nil || e.RevokedBy.String() != agcLeader.ID {
				t.Errorf("revoked row %+v", e)
			}
		}
	}
	if len(entries) != 5 || revoked != 2 {
		t.Fatalf("entries %d revoked %d", len(entries), revoked)
	}
	// Equal scores share a rank: 1, 1, 3.
	set(0, pts(60.125), "")
	board, err = f.svc.Leaderboard(ctx, agcLeader, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	var ranks []int
	for _, r := range board.Rows {
		if r.Rank != nil {
			ranks = append(ranks, *r.Rank)
		}
	}
	if len(ranks) != 4 || ranks[0] != 1 || ranks[1] != 1 || ranks[2] != 3 || ranks[3] != 4 {
		t.Fatalf("ranks %v", ranks)
	}
}

func latest(t *testing.T, f fixture, competitionID, entrantID uuid.UUID) competition.Entry {
	t.Helper()
	entries, err := f.svc.Entries(t.Context(), ykMember, competitionID)
	if err != nil {
		t.Fatal(err)
	}
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].EntrantID == entrantID && entries[i].RevokedAt == nil {
			return entries[i]
		}
	}
	t.Fatalf("no live entry for %s", entrantID)
	return competition.Entry{}
}

func TestServiceScoreRefusals(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t, memoryStore)
	c := f.create(t)
	other := f.create(t)
	added, err := f.svc.AddEntrants(ctx, agcLeader, c.ID, []uuid.UUID{f.guest(t, "Ada", "L").ID})
	if err != nil {
		t.Fatal(err)
	}
	entrant := added[0].ID
	foreignStage := other.Stages[0].ID
	for name, in := range map[string]competition.ScoreInput{
		"unknown entrant":      {EntrantID: uuid.New(), Points: pts(30)},
		"another stage":        {EntrantID: entrant, StageID: &foreignStage, Points: pts(30)},
		"past the upper bound": {EntrantID: entrant, Points: scoring.MaxPoints + 1},
	} {
		if _, err := f.svc.SetScore(ctx, agcLeader, c.ID, in); !errors.Is(err, competition.ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
	if _, err := f.svc.SetScore(ctx, agcMember, c.ID, competition.ScoreInput{EntrantID: entrant, Points: pts(30)}); !errors.Is(err, competition.ErrForbidden) {
		t.Errorf("member: %v", err)
	}
	if _, err := f.svc.Revoke(ctx, agcLeader, c.ID, uuid.New(), "x"); !errors.Is(err, competition.ErrNotFound) {
		t.Errorf("revoke unknown: %v", err)
	}
	if _, err := f.svc.Cancel(ctx, agcLeader, c.ID, "iptal"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.SetScore(ctx, agcLeader, c.ID, competition.ScoreInput{EntrantID: entrant, Points: pts(30)}); !errors.Is(err, competition.ErrStatus) {
		t.Errorf("cancelled: %v", err)
	}
}

// A registered member whose account is being deleted shows as
// "Silinmiş kullanıcı"; the score stays.
func TestServiceShowsADeletedAccountAsDeleted(t *testing.T) {
	t.Parallel()
	f := newFixture(t, memoryStore)
	c := f.create(t)
	tk := f.registered(t, "Ada", "Lovelace")
	if _, err := f.svc.AddEntrants(t.Context(), agcLeader, c.ID, []uuid.UUID{tk.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.users.RequestDeletion(t.Context(), *tk.OwnerID, nil); err != nil {
		t.Fatal(err)
	}
	list, err := f.svc.Entrants(t.Context(), agcLeader, c.ID)
	if err != nil || len(list) != 1 || list[0].DisplayName != user.DeletedDisplayName {
		t.Fatalf("entrants %+v %v", list, err)
	}
}
