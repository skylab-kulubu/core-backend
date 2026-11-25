package event_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/lifecycle"
)

// listWorld: ARTLAB owns a published Event (WEBLAB "leaders" shown,
// GECEKODU "members" helper, a person) and a draft (WEBLAB); ALGOLAB owns
// a published Event with no Co-organizers.
type listWorld struct {
	store                   *event.MemoryStore
	svc                     event.Service
	published, draft, other event.Event
	person                  authz.Principal
	personID                uuid.UUID
}

var (
	weblabMember   = authz.Principal{ID: "wm", Groups: []string{"/UYELER/ARGE/WEBLAB"}}
	gecekoduMember = authz.Principal{ID: "gm", Groups: []string{"/UYELER/ORGANIZASYON/GECEKODU"}}
	algolabMember  = authz.Principal{ID: "am", Groups: []string{"/UYELER/ARGE/ALGOLAB/LIDERLER"}}
	listClock      = func() time.Time { return time.Date(2026, 12, 1, 12, 0, 0, 0, time.UTC) }
)

func newListWorld(t *testing.T) listWorld {
	t.Helper()
	ctx := context.Background()
	store := event.NewMemoryStore()
	svc := event.NewServiceWithOptions(store, authz.NewAuthorizer(authz.DefaultPolicy()), event.ServiceOptions{Teams: organizerTeams, CoOrganizersWrite: true, Now: listClock})
	w := listWorld{store: store, svc: svc, personID: uuid.New()}
	w.person = authz.Principal{ID: w.personID.String()}
	start, end := time.Date(2026, 12, 12, 10, 0, 0, 0, time.UTC), time.Date(2026, 12, 12, 18, 0, 0, 0, time.UTC)
	create := func(owner string, status event.Status) event.Event {
		t.Helper()
		e, err := svc.Create(ctx, ykMember, event.Event{Name: "Hack " + owner, Location: "YTÜ", OwnerTeam: owner, Status: status, StartDate: &start, EndDate: &end})
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	w.published, w.draft, w.other = create("ARTLAB", event.StatusPublished), create("ARTLAB", event.StatusDraft), create("ALGOLAB", event.StatusPublished)
	if _, err := svc.SetOrganizers(ctx, ykMember, w.published.ID, event.OrganizersInput{
		Teams: []event.OrganizerTeamInput{
			{Code: "WEBLAB", Management: ptr(event.ManagementLeaders)},
			{Code: "GECEKODU", Management: ptr(event.ManagementMembers), Visibility: ptr(event.VisibilityHelper)},
		},
		People: []uuid.UUID{w.personID},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetOrganizers(ctx, ykMember, w.draft.ID, event.OrganizersInput{Teams: []event.OrganizerTeamInput{{Code: "WEBLAB"}}}); err != nil {
		t.Fatal(err)
	}
	return w
}

func ids(events []event.Event) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(events))
	for _, e := range events {
		out = append(out, e.ID)
	}
	slices.SortFunc(out, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	return out
}

func sameIDs(t *testing.T, label string, got []event.Event, want ...event.Event) {
	t.Helper()
	if g, w := ids(got), ids(want); !slices.Equal(g, w) {
		t.Errorf("%s: got %v, want %v", label, g, w)
	}
}

// ?team= is a team's Events as Owner team or as a Co-organizer shown
// outside the panel: a helper team is never matched; drafts as ?ownerTeam=
// (only with drafts, only those the caller may see).
func TestListTeamMatchesTheOwnerAndTheShownCoOrganizers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	w := newListWorld(t)

	got, err := w.svc.ListTeam(ctx, nil, "WEBLAB", true, false)
	if err != nil {
		t.Fatal(err)
	}
	sameIDs(t, "signed out, WEBLAB", got, w.published)
	got, _ = w.svc.ListTeam(ctx, &ownerLeader, "WEBLAB", false, true)
	sameIDs(t, "WEBLAB's Leader with drafts", got, w.published, w.draft)
	got, _ = w.svc.ListTeam(ctx, &weblabMember, "WEBLAB", false, true)
	sameIDs(t, "a member of a leaders team with drafts", got, w.published)
	got, _ = w.svc.ListTeam(ctx, &gecekoduMember, "GECEKODU", false, true)
	sameIDs(t, "a helper team", got)
	got, _ = w.svc.ListTeam(ctx, &unrelatedLead, "ARTLAB", false, false)
	sameIDs(t, "the Owner team without drafts", got, w.published)
	got, _ = w.svc.ListTeam(ctx, &unrelatedLead, "ARTLAB", false, true)
	sameIDs(t, "the Owner team's Leader with drafts", got, w.published, w.draft)
	// ?ownerTeam= keeps its meaning: the Owner team alone.
	got, _ = w.svc.List(ctx, &ownerLeader, "WEBLAB", false, true)
	sameIDs(t, "ownerTeam=WEBLAB", got)
}

// ?managed=true is every Event the caller may edit, drafts included,
// decided on each Event: the Owner team's Leader, a Co-organizer team's
// seat, a person; a member of a "leaders" team and anyone else see none. A
// helper team's Events come here, never through ?team=.
func TestListManagedIsWhatTheCallerMayEdit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	w := newListWorld(t)
	for name, c := range map[string]struct {
		p    authz.Principal
		want []event.Event
	}{
		"the Owner team's Leader":       {unrelatedLead, []event.Event{w.published, w.draft}},
		"WEBLAB's Leader (leaders)":     {ownerLeader, []event.Event{w.published, w.draft}},
		"a WEBLAB member (leaders)":     {weblabMember, nil},
		"a GECEKODU member (members)":   {gecekoduMember, []event.Event{w.published}},
		"the person":                    {w.person, []event.Event{w.published}},
		"ALGOLAB's Leader":              {algolabMember, []event.Event{w.other}},
		"Privileged (event:manage, YK)": {ykMember, []event.Event{w.published, w.draft, w.other}},
		"a stranger":                    {authz.Principal{ID: "x"}, nil},
	} {
		got, err := w.svc.ListManaged(ctx, c.p, "", "", false)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		sameIDs(t, name, got, c.want...)
	}
	got, _ := w.svc.ListManaged(ctx, ownerLeader, "", "WEBLAB", false)
	sameIDs(t, "managed and team=WEBLAB", got, w.published, w.draft)
	got, _ = w.svc.ListManaged(ctx, gecekoduMember, "", "GECEKODU", false)
	sameIDs(t, "managed and a helper team", got)
	got, _ = w.svc.ListManaged(ctx, ownerLeader, "ARTLAB", "", false)
	sameIDs(t, "managed and ownerTeam=ARTLAB", got, w.published, w.draft)
	got, _ = w.svc.ListManaged(ctx, ownerLeader, "", "", true)
	sameIDs(t, "managed and active", got, w.published)
}

// The archive is decided with Delete, the Owner team's: a Co-organizer
// does not see an archived Event; its Owner team's Leader does.
func TestTheArchiveStaysTheOwnerTeams(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	w := newListWorld(t)
	if err := w.svc.Delete(ctx, unrelatedLead, w.draft.ID); err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string]authz.Principal{"WEBLAB's Leader": ownerLeader, "the person": w.person} {
		got, err := w.svc.ListLifecycle(ctx, p, "", lifecycle.InactiveOnly)
		if err != nil {
			t.Fatal(err)
		}
		sameIDs(t, name, got)
	}
	got, _ := w.svc.ListLifecycle(ctx, unrelatedLead, "", lifecycle.InactiveOnly)
	sameIDs(t, "the Owner team's Leader", got, w.draft)
}

// A copy brings the Co-organizer teams with their choices; the people only
// when asked and only for who may change the list (named skipped
// otherwise). A Co-organizer copies into the Owner team, never into their
// own, and only while they manage the copy: a person, whose row does not
// come along, may not copy.
func TestCopyCarriesTheCoOrganizers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	w := newListWorld(t)
	start := time.Date(2027, 12, 12, 10, 0, 0, 0, time.UTC)
	copyOf := func(p authz.Principal, in event.CopyInput) (event.Copied, error) {
		t.Helper()
		in.ID, in.Name, in.StartDate = uuid.New(), "Hack 2027", &start
		copied, _, err := w.svc.Copy(ctx, p, w.published.ID, in)
		return copied, err
	}
	wantTeams := []event.CoOrganizerTeam{
		{Code: "GECEKODU", Management: event.ManagementMembers, Visibility: event.VisibilityHelper},
		{Code: "WEBLAB", Management: event.ManagementLeaders, Visibility: event.VisibilityCoOrganizer},
	}

	owner, err := copyOf(unrelatedLead, event.CopyInput{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(owner.Event.CoOrganizers, wantTeams) || owner.Event.CoOrganizerIDs != nil || len(owner.Skipped) != 0 {
		t.Fatalf("owner's copy without people: %+v %v %v", owner.Event.CoOrganizers, owner.Event.CoOrganizerIDs, owner.Skipped)
	}
	withPeople, err := copyOf(unrelatedLead, event.CopyInput{IncludeCoOrganizerPeople: true})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(withPeople.Event.CoOrganizerIDs, []uuid.UUID{w.personID}) {
		t.Fatalf("owner's copy with people: %v", withPeople.Event.CoOrganizerIDs)
	}

	co, err := copyOf(ownerLeader, event.CopyInput{IncludeCoOrganizerPeople: true})
	if err != nil {
		t.Fatalf("WEBLAB's Leader does not copy: %v", err)
	}
	if co.Event.OwnerTeam != "ARTLAB" || !slices.Equal(co.Event.CoOrganizers, wantTeams) || co.Event.CoOrganizerIDs != nil ||
		!slices.Equal(co.Skipped, []string{event.SkippedCoOrganizerPeople}) {
		t.Fatalf("WEBLAB's Leader's copy: %s %+v %v %v", co.Event.OwnerTeam, co.Event.CoOrganizers, co.Event.CoOrganizerIDs, co.Skipped)
	}
	if _, err := w.svc.Get(ctx, &ownerLeader, co.Event.ID); err != nil {
		t.Fatalf("WEBLAB's Leader does not see their copy: %v", err)
	}
	if _, err := copyOf(ownerLeader, event.CopyInput{OwnerTeam: "WEBLAB"}); !errors.Is(err, event.ErrForbidden) {
		t.Errorf("WEBLAB's Leader copies into WEBLAB: %v", err)
	}
	if _, err := copyOf(gecekoduMember, event.CopyInput{}); err != nil {
		t.Errorf("a member of a members team does not copy: %v", err)
	}
	if _, err := copyOf(w.person, event.CopyInput{IncludeCoOrganizerPeople: true}); !errors.Is(err, event.ErrForbidden) {
		t.Errorf("the person copies: %v", err)
	}
	if _, err := copyOf(weblabMember, event.CopyInput{}); !errors.Is(err, event.ErrForbidden) {
		t.Errorf("a member of a leaders team copies: %v", err)
	}

	// While EVENT_CO_ORGANIZERS_WRITE is off a copy has no Co-organizers, as before.
	off := event.NewServiceWithOptions(w.store, authz.NewAuthorizer(authz.DefaultPolicy()), event.ServiceOptions{Teams: organizerTeams, Now: listClock})
	copied, _, err := off.Copy(ctx, unrelatedLead, w.published.ID, event.CopyInput{ID: uuid.New(), Name: "Off", StartDate: &start, IncludeCoOrganizerPeople: true})
	if err != nil {
		t.Fatal(err)
	}
	if copied.Event.CoOrganizers != nil || copied.Event.CoOrganizerIDs != nil || !slices.Equal(copied.Skipped, []string{event.SkippedCoOrganizerPeople}) {
		t.Fatalf("copy while off: %+v %v %v", copied.Event.CoOrganizers, copied.Event.CoOrganizerIDs, copied.Skipped)
	}
}
