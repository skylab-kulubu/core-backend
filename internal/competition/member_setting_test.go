package competition_test

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/competition"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// allMembers is AGC with its setting "Yarışmaları kim yönetir" on all
// members (authz.TeamAttrCompetitionManage).
type allMembers struct{}

func (allMembers) MembersManageCompetitors(string) bool { return true }

func (allMembers) TeamCompetitionManage(team string) bool { return team == "AGC" }

func TestServiceMemberSettingBasisAndOwnEntrant(t *testing.T) {
	t.Parallel()
	runMemberSetting(t, newFixture(t, memoryStore))
}

func TestPostgresMemberSettingBasisAndOwnEntrant(t *testing.T) {
	f, _ := postgresFixture(t)
	runMemberSetting(t, f)
}

// owned is a registered Ticket of the person p.
func (f fixture) owned(t *testing.T, p authz.Principal) ticket.Ticket {
	t.Helper()
	id := uuid.MustParse(p.ID)
	if _, err := f.users.Get(t.Context(), id); errors.Is(err, user.ErrNotFound) {
		if _, _, err := f.users.Upsert(t.Context(), user.User{ID: id, Email: p.ID + "@example.test", FirstName: "Org"}); err != nil {
			t.Fatal(err)
		}
	}
	tk, err := f.tickets.Create(t.Context(), ticket.Ticket{EventID: f.event.ID, TicketType: ticket.Registered, OwnerID: &id})
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

// With AGC's setting on, a member manages AGC's Competition but does not
// archive it; every ledger row keeps its writer's and revoker's basis;
// nobody, Leader and Privileged included, writes or revokes a score of an
// Entrant they are in (A3).
func runMemberSetting(t *testing.T, f fixture) {
	t.Helper()
	ctx := t.Context()
	f = f.decidedBy(authz.NewAuthorizer(authz.DefaultPolicy(), authz.WithTeamSettings(allMembers{})))

	c, err := f.svc.Create(ctx, agcMember, f.event.ID, competition.CreateInput{Name: "AGC Hafta 4"})
	if err != nil {
		t.Fatalf("a member creates: %v", err)
	}
	leaderTicket, memberTicket, guest := f.owned(t, agcLeader), f.owned(t, agcMember), f.guest(t, "Grace", "Hopper")
	entrants, err := f.svc.AddEntrants(ctx, agcMember, c.ID, []uuid.UUID{leaderTicket.ID, memberTicket.ID})
	if err != nil {
		t.Fatalf("a member adds entrants: %v", err)
	}
	leaderEntrant, memberEntrant := entrants[0].ID, entrants[1].ID

	// The candidates: names and check-in only, the Entrants marked.
	cands, err := f.svc.EntrantCandidates(ctx, agcMember, c.ID, competition.CandidateFilter{})
	if err != nil || len(cands) != 3 {
		t.Fatalf("candidates %+v %v", cands, err)
	}
	marked := map[uuid.UUID]bool{}
	for _, cand := range cands {
		marked[cand.TicketID] = cand.Entrant
		if cand.CheckedIn {
			t.Errorf("checked in: %+v", cand)
		}
	}
	if !marked[leaderTicket.ID] || !marked[memberTicket.ID] || marked[guest.ID] {
		t.Errorf("entrant marks %v", marked)
	}
	raw, err := json.Marshal(cands[0])
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if want := []string{"checkedIn", "displayName", "entrant", "ticketId", "ticketType"}; !slices.Equal(keys, want) {
		t.Errorf("a candidate answers %v, want %v", keys, want)
	}
	if got, err := f.svc.EntrantCandidates(ctx, agcMember, c.ID, competition.CandidateFilter{Query: "hopp"}); err != nil ||
		len(got) != 1 || got[0].TicketID != guest.ID || got[0].DisplayName != "Grace Hopper" || got[0].TicketType != ticket.Guest {
		t.Errorf("q=hopp: %+v %v", got, err)
	}
	yes := true
	if got, err := f.svc.EntrantCandidates(ctx, agcMember, c.ID, competition.CandidateFilter{CheckedIn: &yes}); err != nil || len(got) != 0 {
		t.Errorf("checkedIn=true: %+v %v", got, err)
	}
	outsider := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/WEBLAB"}}
	if _, err := f.svc.EntrantCandidates(ctx, outsider, c.ID, competition.CandidateFilter{}); !errors.Is(err, competition.ErrForbidden) {
		t.Errorf("another Team's member reads candidates: %v", err)
	}
	if guestEntrant, err := f.svc.AddEntrants(ctx, agcMember, c.ID, []uuid.UUID{guest.ID}); err != nil || len(guestEntrant) != 1 {
		t.Fatalf("add the guest: %v", err)
	}

	set := func(p authz.Principal, entrant uuid.UUID, replaces *uuid.UUID, points float64, reason string) (competition.Entry, error) {
		return f.svc.SetScore(ctx, p, c.ID, competition.ScoreInput{EntrantID: entrant, ReplacesID: replaces, Points: pts(points), Reason: reason})
	}
	// The member scores the Leader; the Leader corrects it.
	first, err := set(agcMember, leaderEntrant, nil, 30, "")
	if err != nil || first.Basis != string(authz.BasisMemberSetting) {
		t.Fatalf("a member's score: %+v %v", first, err)
	}
	if _, err := set(agcLeader, leaderEntrant, &first.ID, 50, "yanlış"); !errors.Is(err, competition.ErrOwnEntrant) {
		t.Errorf("the Leader corrects their own score: %v", err)
	}
	if _, err := f.svc.Revoke(ctx, agcLeader, c.ID, first.ID, "yanlış"); !errors.Is(err, competition.ErrOwnEntrant) {
		t.Errorf("the Leader revokes their own score: %v", err)
	}
	if _, err := set(agcMember, memberEntrant, nil, 60, ""); !errors.Is(err, competition.ErrOwnEntrant) {
		t.Errorf("a member scores themselves: %v", err)
	}
	memberScore, err := set(agcLeader, memberEntrant, nil, 60, "")
	if err != nil || memberScore.Basis != string(authz.BasisLeader) {
		t.Fatalf("the Leader's score: %+v %v", memberScore, err)
	}
	corrected, err := set(agcMember, leaderEntrant, &first.ID, 50, "yeniden sayıldı")
	if err != nil || corrected.Basis != string(authz.BasisMemberSetting) {
		t.Fatalf("a member's correction: %+v %v", corrected, err)
	}
	revoked, err := f.svc.Revoke(ctx, ykMember, c.ID, corrected.ID, "itiraz")
	if err != nil || revoked.RevokedBasis != string(authz.BasisPrivileged) {
		t.Fatalf("YK revokes: %+v %v", revoked, err)
	}

	ledger, err := f.svc.Entries(ctx, agcMember, c.ID)
	if err != nil || len(ledger) != 3 {
		t.Fatalf("ledger %+v %v", ledger, err)
	}
	byID := map[uuid.UUID]competition.Entry{}
	for _, row := range ledger {
		byID[row.ID] = row
	}
	if row := byID[first.ID]; row.Basis != "member_setting" || row.RevokedBasis != "member_setting" {
		t.Errorf("first row kept %q, revoked %q", row.Basis, row.RevokedBasis)
	}
	if row := byID[memberScore.ID]; row.Basis != "leader" || row.RevokedBasis != "" {
		t.Errorf("the Leader's row kept %q, revoked %q", row.Basis, row.RevokedBasis)
	}
	if row := byID[corrected.ID]; row.Basis != "member_setting" || row.RevokedBasis != "privileged" {
		t.Errorf("the correction kept %q, revoked %q", row.Basis, row.RevokedBasis)
	}

	// Archive and restore stay the organizer's.
	if err := f.svc.Archive(ctx, agcMember, c.ID); !errors.Is(err, competition.ErrForbidden) {
		t.Errorf("a member archives: %v", err)
	}
	if err := f.svc.Archive(ctx, agcLeader, c.ID); err != nil {
		t.Fatalf("the Leader archives: %v", err)
	}
	if _, err := f.svc.Restore(ctx, agcMember, c.ID); !errors.Is(err, competition.ErrForbidden) {
		t.Errorf("a member restores: %v", err)
	}
}

// With the setting off (the default) a member manages nothing, the
// candidates neither.
func TestServiceMemberSettingOff(t *testing.T) {
	t.Parallel()
	f := newFixture(t, memoryStore)
	c := f.create(t)
	if _, err := f.svc.EntrantCandidates(t.Context(), agcMember, c.ID, competition.CandidateFilter{}); !errors.Is(err, competition.ErrForbidden) {
		t.Errorf("candidates: %v", err)
	}
	if _, err := f.svc.Create(t.Context(), agcMember, f.event.ID, competition.CreateInput{Name: "x"}); !errors.Is(err, competition.ErrForbidden) {
		t.Errorf("create: %v", err)
	}
	if _, err := f.svc.EntrantCandidates(t.Context(), agcLeader, c.ID, competition.CandidateFilter{Query: string(make([]rune, 101))}); !errors.Is(err, competition.ErrInvalid) {
		t.Errorf("a long q: %v", err)
	}
}
