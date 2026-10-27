package membershipterm_test

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/membershipterm"
	"github.com/skylab-kulubu/core-backend/internal/season"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
)

func refusal(t *testing.T, err error, status int, code string) {
	t.Helper()
	var refused *membershipterm.RefusalError
	if !errors.As(err, &refused) || refused.Status != status || refused.Code != code {
		t.Fatalf("err %v, want %d %s", err, status, code)
	}
}

func day(t *testing.T, raw string) time.Time {
	t.Helper()
	at, err := membershipterm.ParseDay(raw)
	if err != nil {
		t.Fatal(err)
	}
	return at
}

// The routes: an add through core starts a term (none for ADMIN or an
// ordinary Team); nobody extends their own term; an extension is bounded
// and clears the mark; only an expired term is removed through its route,
// and that removal goes through the member route's way; a handover adds
// the successor and takes the holder out.
func TestTermRoutes(t *testing.T) {
	f := newFixture(t, membershipterm.ModeApply)
	p := f.people
	f.run(t, membershipterm.ModeApply)
	ctxYK1, yk1 := as(p.yk1, "/UYELER/YK")
	ctxYK2, yk2 := as(p.yk2, "/UYELER/YK")
	ctxAdmin, admin := as(p.admin, "/ADMIN")

	// An add through core: a term for DK, none for ADMIN or a Team.
	if err := f.identity.AddMember(ctxYK1, yk1, gDK.String(), p.newcomer); err != nil {
		t.Fatal(err)
	}
	if err := f.identity.AddMember(ctxYK1, yk1, gWeblab.String(), p.newcomer); err != nil {
		t.Fatal(err)
	}
	if err := f.identity.AddMember(ctxAdmin, admin, gAdmin.String(), p.newcomer); err != nil {
		t.Fatal(err)
	}
	added := f.termOf(t, p.newcomer, gDK)
	if added.Source != membershipterm.SourceRoute || added.GrantedBy == nil || *added.GrantedBy != p.yk1 || added.EndsAt == nil || !added.EndsAt.Equal(termEnd) {
		t.Fatalf("added %+v", added)
	}
	terms, _ := f.store.OpenFor(context.Background(), p.newcomer)
	if len(terms) != 1 {
		t.Fatalf("newcomer's terms %+v", terms)
	}

	yk1Term := f.termOf(t, p.yk1, gYK)
	yk2Term := f.termOf(t, p.yk2, gYK)
	// Nobody extends their own term.
	if _, err := f.svc.Extend(ctxYK1, yk1, yk1Term.ID, day(t, "2027-09-30")); err == nil {
		t.Fatal("extended own term")
	} else {
		refusal(t, err, http.StatusForbidden, membershipterm.CodeSelfExtension)
	}
	// No next season: at most a year on.
	_, err := f.svc.Extend(ctxYK2, yk2, yk1Term.ID, day(t, "2027-12-31"))
	refusal(t, err, http.StatusUnprocessableEntity, membershipterm.CodeTermTooLong)
	if _, err := f.svc.Extend(ctxYK2, yk2, yk1Term.ID, day(t, "2026-10-01")); !errors.Is(err, membershipterm.ErrInvalid) {
		t.Fatalf("a past end: %v", err)
	}
	// A Team member may not change YK's membership.
	ctxMember, member := as(p.member)
	if _, err := f.svc.Extend(ctxMember, member, yk1Term.ID, day(t, "2027-09-30")); !errors.Is(err, identity.ErrForbidden) {
		t.Fatalf("a member extended: %v", err)
	}
	extended, err := f.svc.Extend(ctxYK2, yk2, yk1Term.ID, day(t, "2027-09-30"))
	if err != nil || extended.EndsAt == nil || !extended.EndsAt.Equal(day(t, "2027-09-30")) || extended.FirstName != "Yekta" {
		t.Fatalf("extended %+v %v", extended, err)
	}
	if rows := f.auditRows(t, teamaudit.ActionTermExtend); len(rows) != 1 || rows[0]["actor"] != p.yk2.String() || rows[0]["basis"] != "privileged" ||
		rows[0]["target"] != p.yk1.String() || rows[0]["outcome"] != "ok" || rows[0]["endsAt"] != "2027-09-30T20:59:59Z" {
		t.Fatalf("extend rows %v", rows)
	}

	// Only an expired term is removed through its route.
	refusal(t, f.svc.Remove(ctxYK1, yk1, yk2Term.ID), http.StatusConflict, membershipterm.CodeTermNotExpired)

	f.setNow(termEnd.Add(time.Second))
	run := f.run(t, membershipterm.ModeApply)
	if run.Expired != 5 { // yk2, dk1, lead, sublead, newcomer's DK; yk1 was extended
		t.Fatalf("expiry %+v", run)
	}
	if got, err := f.svc.ExpiredFor(context.Background(), p.yk1); err != nil || len(got) != 0 {
		t.Fatalf("yk1 expired %v %v", got, err)
	}
	if got, err := f.svc.ExpiredFor(context.Background(), p.yk2); err != nil || len(got) != 1 || got[0].GroupID != gYK || got[0].Role != "board" {
		t.Fatalf("yk2 expired %v %v", got, err)
	}
	expired, err := f.svc.List(ctxYK1, yk1, membershipterm.Filter{Status: membershipterm.StatusExpired, Role: membershipterm.RoleBoard})
	if err != nil || len(expired) != 1 || expired[0].UserID != p.yk2 {
		t.Fatalf("expired YK %+v %v", expired, err)
	}
	// A Team member lists nothing: they change no membership with a term.
	if mine, err := f.svc.List(ctxMember, member, membershipterm.Filter{}); err != nil || len(mine) != 0 {
		t.Fatalf("a member's list %+v %v", mine, err)
	}

	// Extending an expired term takes the mark away.
	again, err := f.svc.Extend(ctxYK1, yk1, yk2Term.ID, day(t, "2027-09-30"))
	if err != nil || again.Status != membershipterm.StatusActive || again.ExpiredAt != nil || again.Warned14d != nil || again.Warned1d != nil {
		t.Fatalf("extended expired %+v %v", again, err)
	}

	// Removing DK's expired member: through the member route's way.
	dk1Term := f.termOf(t, p.dk1, gDK)
	if err := f.svc.Remove(ctxYK1, yk1, dk1Term.ID); err != nil {
		t.Fatal(err)
	}
	members, _ := f.dir.Members(context.Background(), gDK.String())
	if slices.ContainsFunc(members, func(m identity.Person) bool { return m.ID == p.dk1 }) {
		t.Fatal("dk1 is still in DK")
	}
	if ended, err := f.store.Get(context.Background(), dk1Term.ID); err != nil || ended.EndedAt == nil {
		t.Fatalf("dk1's term %+v %v", ended, err)
	}
	removed := f.auditRows(t, teamaudit.ActionMemberRemove)
	if len(removed) != 1 || removed[0]["target"] != p.dk1.String() || removed[0]["outcome"] != "ok" {
		t.Fatalf("member.remove rows %v", removed)
	}
	if err := f.svc.Remove(ctxYK1, yk1, dk1Term.ID); !errors.Is(err, membershipterm.ErrNotFound) {
		t.Fatalf("removed twice: %v", err)
	}

	// A handover of WEBLAB's leadership to its member, at once; a next
	// season now bounds the successor's end.
	start, end := time.Date(2027, 6, 1, 0, 0, 0, 0, time.UTC), time.Date(2028, 5, 31, 0, 0, 0, 0, time.UTC)
	if _, err := f.seasons.Create(context.Background(), season.Season{ID: uuid.New(), Name: "2027-2028", StartDate: &start, EndDate: &end}); err != nil {
		t.Fatal(err)
	}
	leadTerm := f.termOf(t, p.lead, gWeblabL)
	tooLate := day(t, "2028-06-15")
	_, err = f.svc.Handover(ctxYK1, yk1, leadTerm.ID, membershipterm.HandoverInput{UserID: p.member, Mode: membershipterm.HandoverImmediate, EndsAt: &tooLate})
	refusal(t, err, http.StatusUnprocessableEntity, membershipterm.CodeTermTooLong)
	_, err = f.svc.Handover(ctxYK1, yk1, leadTerm.ID, membershipterm.HandoverInput{UserID: p.yk1, Mode: membershipterm.HandoverImmediate})
	refusal(t, err, http.StatusForbidden, membershipterm.CodeSelfExtension)
	ends := day(t, "2028-05-31")
	successor, err := f.svc.Handover(ctxYK1, yk1, leadTerm.ID, membershipterm.HandoverInput{UserID: p.member, Mode: membershipterm.HandoverImmediate, EndsAt: &ends})
	if err != nil || successor.UserID != p.member || successor.EndsAt == nil || !successor.EndsAt.Equal(ends) || successor.SeasonID == nil {
		t.Fatalf("successor %+v %v", successor, err)
	}
	leaders, _ := f.dir.Members(context.Background(), gWeblabL.String())
	if len(leaders) != 1 || leaders[0].ID != p.member {
		t.Fatalf("WEBLAB's Leaders %v", leaders)
	}
	if ended, _ := f.store.Get(context.Background(), leadTerm.ID); ended.EndedAt == nil {
		t.Fatal("the holder's term is open")
	}
	handovers := f.auditRows(t, teamaudit.ActionTermHandover)
	if len(handovers) != 1 || handovers[0]["target"] != p.lead.String() || handovers[0]["successor"] != p.member.String() || handovers[0]["outcome"] != "ok" {
		t.Fatalf("handover rows %v", handovers)
	}
}
