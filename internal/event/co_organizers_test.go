package event_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
)

// fakeTeams is the Group tree's answer: each code's one Group path.
type fakeTeams map[string]string

func (f fakeTeams) TeamPath(team string) (string, bool) {
	for code, path := range f {
		if strings.EqualFold(code, strings.TrimSpace(team)) {
			return path, true
		}
	}
	return "", false
}

func (f fakeTeams) SubTeams(string) []string { return nil }

var organizerTeams = fakeTeams{
	"WEBLAB":   "/UYELER/ARGE/WEBLAB",
	"ARTLAB":   "/UYELER/SANAT/ARTLAB",
	"GECEKODU": "/UYELER/ORGANIZASYON/GECEKODU",
	"ALGOLAB":  "/UYELER/ARGE/ALGOLAB",
	"MOBILLAB": "/UYELER/ARGE/MOBILLAB",
	"OYUNLAB":  "/UYELER/ARGE/OYUNLAB",
	"VERILAB":  "/UYELER/ARGE/VERILAB",
	"SIBERLAB": "/UYELER/ARGE/SIBERLAB",
	"ROBOLAB":  "/UYELER/ARGE/ROBOLAB",
	"BULUTLAB": "/UYELER/ARGE/BULUTLAB",
	"YAPAYLAB": "/UYELER/ARGE/YAPAYLAB",
	"ARGE":     "/UYELER/ARGE",
	"YK":       "/UYELER/YK",
}

var (
	ykMember      = authz.Principal{ID: "yk", Groups: []string{"/UYELER/YK"}}
	dkMember      = authz.Principal{ID: "dk", Groups: []string{"/UYELER/DK"}}
	ownerLeader   = authz.Principal{ID: "wl", Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
	unrelatedLead = authz.Principal{ID: "al", Groups: []string{"/UYELER/SANAT/ARTLAB/LIDERLER"}}
)

func organizerSetup(t *testing.T) (*event.MemoryStore, event.Service) {
	t.Helper()
	store := event.NewMemoryStore()
	return store, event.NewServiceWithOptions(store, authz.NewAuthorizer(authz.DefaultPolicy()), event.ServiceOptions{Teams: organizerTeams, CoOrganizersWrite: true})
}

func createEvent(t *testing.T, svc event.Service, owner string, status event.Status) event.Event {
	t.Helper()
	end := time.Date(2026, 12, 12, 18, 0, 0, 0, time.UTC)
	e, err := svc.Create(context.Background(), ykMember, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: owner, Status: status, EndDate: &end})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func ptr[T any](v T) *T { return &v }

// A valid list is stored; a team's left-out choices take the Owner team's
// rule (YK: leaders; GECEKODU: members) and co_organizer; codes are stored
// as their Groups are named; GET answers the same, with each person's end.
func TestOrganizersSetAndDefaults(t *testing.T) {
	t.Parallel()
	_, svc := organizerSetup(t)
	ctx := context.Background()
	person := uuid.New()

	yk := createEvent(t, svc, "YK", event.StatusPublished)
	got, err := svc.SetOrganizers(ctx, ykMember, yk.ID, event.OrganizersInput{
		Teams: []event.OrganizerTeamInput{
			{Code: " weblab "},
			{Code: "ARTLAB", Management: ptr(event.ManagementMembers), Visibility: ptr(event.VisibilityHelper)},
		},
		People: []uuid.UUID{person},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []event.OrganizerTeam{
		{Code: "ARTLAB", Name: "ARTLAB", Management: event.ManagementMembers, Visibility: event.VisibilityHelper},
		{Code: "WEBLAB", Name: "WEBLAB", Management: event.ManagementLeaders, Visibility: event.VisibilityCoOrganizer},
	}
	if got.OwnerTeam != "YK" || got.OwnerRule != event.ManagementLeaders || !slices.Equal(got.Teams, want) || !got.CanManage {
		t.Fatalf("answer %+v", got)
	}
	expires := time.Date(2027, 1, 11, 18, 0, 0, 0, time.UTC)
	if len(got.People) != 1 || got.People[0].ID != person || got.People[0].ExpiresAt == nil || !got.People[0].ExpiresAt.Equal(expires) {
		t.Fatalf("people %+v", got.People)
	}
	read, err := svc.Organizers(ctx, ykMember, yk.ID)
	if err != nil || !slices.Equal(read.Teams, want) || len(read.People) != 1 {
		t.Fatalf("GET %+v %v", read, err)
	}

	gecekodu := createEvent(t, svc, "GECEKODU", event.StatusPublished)
	got, err = svc.SetOrganizers(ctx, ykMember, gecekodu.ID, event.OrganizersInput{Teams: []event.OrganizerTeamInput{{Code: "WEBLAB"}}})
	if err != nil {
		t.Fatal(err)
	}
	if got.OwnerRule != event.ManagementMembers || got.Teams[0].Management != event.ManagementMembers || got.Teams[0].Visibility != event.VisibilityCoOrganizer {
		t.Fatalf("GECEKODU's default %+v", got)
	}

	// The whole list each time: an empty one clears it.
	got, err = svc.SetOrganizers(ctx, ykMember, yk.ID, event.OrganizersInput{})
	if err != nil || len(got.Teams) != 0 || len(got.People) != 0 {
		t.Fatalf("cleared %+v %v", got, err)
	}
}

// Each check refuses alone, naming the field, the item and why.
func TestOrganizersRefusals(t *testing.T) {
	t.Parallel()
	_, svc := organizerSetup(t)
	ctx := context.Background()
	e := createEvent(t, svc, "WEBLAB", event.StatusPublished)
	person := uuid.New()
	var eleven []event.OrganizerTeamInput
	for code := range organizerTeams {
		if code != "WEBLAB" && code != "ARGE" && code != "YK" {
			eleven = append(eleven, event.OrganizerTeamInput{Code: code})
		}
	}
	eleven = append(eleven, event.OrganizerTeamInput{Code: "X"})
	people := make([]uuid.UUID, 11)
	for i := range people {
		people[i] = uuid.New()
	}
	cases := []struct {
		name string
		in   event.OrganizersInput
		want event.OrganizersError
	}{
		{"the Owner team", event.OrganizersInput{Teams: []event.OrganizerTeamInput{{Code: "webLAB"}}}, event.OrganizersError{Field: "teams", Index: 0, Reason: event.OrganizersOwnerTeam}},
		{"an unknown team", event.OrganizersInput{Teams: []event.OrganizerTeamInput{{Code: "ARTLAB"}, {Code: "NOPE"}}}, event.OrganizersError{Field: "teams", Index: 1, Reason: event.OrganizersUnknownTeam}},
		{"an area", event.OrganizersInput{Teams: []event.OrganizerTeamInput{{Code: "ARGE"}}}, event.OrganizersError{Field: "teams", Index: 0, Reason: event.OrganizersUnknownTeam}},
		{"YK", event.OrganizersInput{Teams: []event.OrganizerTeamInput{{Code: "yk"}}}, event.OrganizersError{Field: "teams", Index: 0, Reason: event.OrganizersPrivilegedTeam}},
		{"DK", event.OrganizersInput{Teams: []event.OrganizerTeamInput{{Code: "DK"}}}, event.OrganizersError{Field: "teams", Index: 0, Reason: event.OrganizersPrivilegedTeam}},
		{"ADMIN", event.OrganizersInput{Teams: []event.OrganizerTeamInput{{Code: "ADMIN"}}}, event.OrganizersError{Field: "teams", Index: 0, Reason: event.OrganizersPrivilegedTeam}},
		{"a blank code", event.OrganizersInput{Teams: []event.OrganizerTeamInput{{Code: " "}}}, event.OrganizersError{Field: "teams", Index: 0, Reason: event.OrganizersInvalidCode}},
		{"a path", event.OrganizersInput{Teams: []event.OrganizerTeamInput{{Code: "/UYELER/SANAT/ARTLAB"}}}, event.OrganizersError{Field: "teams", Index: 0, Reason: event.OrganizersInvalidCode}},
		{"a team twice", event.OrganizersInput{Teams: []event.OrganizerTeamInput{{Code: "ARTLAB"}, {Code: "artlab"}}}, event.OrganizersError{Field: "teams", Index: 1, Reason: event.OrganizersDuplicate}},
		{"eleven teams", event.OrganizersInput{Teams: eleven}, event.OrganizersError{Field: "teams", Index: -1, Reason: event.OrganizersTooMany}},
		{"an unknown management", event.OrganizersInput{Teams: []event.OrganizerTeamInput{{Code: "ARTLAB", Management: ptr(event.Management("everyone"))}}}, event.OrganizersError{Field: "teams", Index: 0, Reason: event.OrganizersInvalidManagement}},
		{"an unknown visibility", event.OrganizersInput{Teams: []event.OrganizerTeamInput{{Code: "ARTLAB", Visibility: ptr(event.Visibility("public"))}}}, event.OrganizersError{Field: "teams", Index: 0, Reason: event.OrganizersInvalidVisibility}},
		{"a person twice", event.OrganizersInput{People: []uuid.UUID{person, person}}, event.OrganizersError{Field: "people", Index: 1, Reason: event.OrganizersDuplicate}},
		{"no person", event.OrganizersInput{People: []uuid.UUID{uuid.Nil}}, event.OrganizersError{Field: "people", Index: 0, Reason: event.OrganizersInvalidPerson}},
		{"eleven people", event.OrganizersInput{People: people}, event.OrganizersError{Field: "people", Index: -1, Reason: event.OrganizersTooMany}},
	}
	for _, c := range cases {
		_, err := svc.SetOrganizers(ctx, ykMember, e.ID, c.in)
		var refused *event.OrganizersError
		if !errors.As(err, &refused) || *refused != c.want || !errors.Is(err, event.ErrInvalid) {
			t.Errorf("%s: %v, want %+v", c.name, err, c.want)
		}
	}
	if got, err := svc.Organizers(ctx, ykMember, e.ID); err != nil || len(got.Teams)+len(got.People) != 0 {
		t.Fatalf("a refused list was written: %+v %v", got, err)
	}

	// Without the Group tree no team is known.
	plain := event.NewServiceWithOptions(event.NewMemoryStore(), authz.NewAuthorizer(authz.DefaultPolicy()), event.ServiceOptions{CoOrganizersWrite: true})
	other := createEvent(t, plain, "WEBLAB", event.StatusPublished)
	_, err := plain.SetOrganizers(ctx, ykMember, other.ID, event.OrganizersInput{Teams: []event.OrganizerTeamInput{{Code: "ARTLAB"}}})
	var refused *event.OrganizersError
	if !errors.As(err, &refused) || refused.Reason != event.OrganizersUnknownTeam {
		t.Fatalf("without the tree: %v", err)
	}
}

// Who: the list is changed by event:manage alone for now (the Owner team's
// Leader, DK and anyone else are 403); it is read by who may edit the
// Event; a draft others may not see is 404.
func TestOrganizersWho(t *testing.T) {
	t.Parallel()
	_, svc := organizerSetup(t)
	ctx := context.Background()
	e := createEvent(t, svc, "WEBLAB", event.StatusPublished)
	list := event.OrganizersInput{Teams: []event.OrganizerTeamInput{{Code: "ARTLAB"}}}
	for name, p := range map[string]authz.Principal{"the Owner team's Leader": ownerLeader, "DK": dkMember, "another team's Leader": unrelatedLead} {
		if _, err := svc.SetOrganizers(ctx, p, e.ID, list); !errors.Is(err, event.ErrForbidden) {
			t.Errorf("PUT by %s: %v", name, err)
		}
	}
	if _, err := svc.SetOrganizers(ctx, ykMember, e.ID, list); err != nil {
		t.Fatal(err)
	}
	got, err := svc.Organizers(ctx, ownerLeader, e.ID)
	if err != nil || got.CanManage || len(got.Teams) != 1 {
		t.Fatalf("GET by the Owner team's Leader: %+v %v", got, err)
	}
	for name, p := range map[string]authz.Principal{"DK": dkMember, "another team's Leader": unrelatedLead} {
		if _, err := svc.Organizers(ctx, p, e.ID); !errors.Is(err, event.ErrForbidden) {
			t.Errorf("GET by %s: %v", name, err)
		}
	}
	draft := createEvent(t, svc, "WEBLAB", event.StatusDraft)
	if _, err := svc.Organizers(ctx, unrelatedLead, draft.ID); !errors.Is(err, event.ErrNotFound) {
		t.Fatalf("a draft's GET by another team's Leader: %v", err)
	}
	if _, err := svc.SetOrganizers(ctx, ykMember, uuid.New(), list); !errors.Is(err, event.ErrNotFound) {
		t.Fatalf("no Event: %v", err)
	}
}

// An update of the Event never writes the list; one that makes a
// Co-organizer team the Owner team drops it from the list.
func TestOrganizersEventUpdate(t *testing.T) {
	t.Parallel()
	_, svc := organizerSetup(t)
	ctx := context.Background()
	e := createEvent(t, svc, "WEBLAB", event.StatusPublished)
	person := uuid.New()
	if _, err := svc.SetOrganizers(ctx, ykMember, e.ID, event.OrganizersInput{
		Teams: []event.OrganizerTeamInput{{Code: "ARTLAB"}, {Code: "GECEKODU"}}, People: []uuid.UUID{person},
	}); err != nil {
		t.Fatal(err)
	}
	updated, err := svc.Update(ctx, ykMember, e.ID, event.Event{Name: "Hack 2", Location: "YTÜ", OwnerTeam: "WEBLAB", CoOrganizers: []event.CoOrganizerTeam{{Code: "OYUNLAB"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.CoOrganizers) != 2 || !slices.Equal(updated.CoOrganizerIDs, []uuid.UUID{person}) {
		t.Fatalf("an update wrote the list: %+v %v", updated.CoOrganizers, updated.CoOrganizerIDs)
	}
	updated, err = svc.Update(ctx, ykMember, e.ID, event.Event{Name: "Hack 2", Location: "YTÜ", OwnerTeam: "ARTLAB"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(updated.CoOrganizers, []event.CoOrganizerTeam{{Code: "GECEKODU", Management: event.ManagementLeaders, Visibility: event.VisibilityCoOrganizer}}) {
		t.Fatalf("after the Owner team became ARTLAB: %+v", updated.CoOrganizers)
	}
	got, err := svc.Organizers(ctx, ykMember, e.ID)
	if err != nil || got.OwnerTeam != "ARTLAB" || len(got.Teams) != 1 || len(got.People) != 1 {
		t.Fatalf("GET %+v %v", got, err)
	}
}

// The Event's JSON: anyone sees the co_organizer teams alone; the whole
// lists, helper teams and people, only who may edit it.
func TestOrganizersInEventJSON(t *testing.T) {
	t.Parallel()
	_, svc := organizerSetup(t)
	ctx := context.Background()
	e := createEvent(t, svc, "YK", event.StatusPublished)
	person := uuid.New()
	if _, err := svc.SetOrganizers(ctx, ykMember, e.ID, event.OrganizersInput{
		Teams: []event.OrganizerTeamInput{{Code: "WEBLAB"}, {Code: "GECEKODU", Visibility: ptr(event.VisibilityHelper)}}, People: []uuid.UUID{person},
	}); err != nil {
		t.Fatal(err)
	}
	read, err := svc.Get(ctx, nil, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	decode := func(p *authz.Principal) map[string]any {
		raw, err := json.Marshal(svc.ProjectFor(p, read))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "GECEKODU") != (p != nil && p.ID == ykMember.ID) || strings.Contains(string(raw), person.String()) != (p != nil && p.ID == ykMember.ID) {
			t.Errorf("%v: %s", p, raw)
		}
		var out map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	for _, p := range []*authz.Principal{nil, &dkMember, &unrelatedLead} {
		out := decode(p)
		if teams, _ := out["coOrganizerTeams"].([]any); len(teams) != 1 || teams[0] != "WEBLAB" {
			t.Errorf("%v: coOrganizerTeams %v", p, out["coOrganizerTeams"])
		}
		if _, has := out["coOrganizers"]; has {
			t.Errorf("%v sees coOrganizers", p)
		}
		if _, has := out["coOrganizerIds"]; has {
			t.Errorf("%v sees coOrganizerIds", p)
		}
	}
	out := decode(&ykMember)
	if teams, _ := out["coOrganizers"].([]any); len(teams) != 2 {
		t.Errorf("the editor's coOrganizers %v", out["coOrganizers"])
	}
	if ids, _ := out["coOrganizerIds"].([]any); len(ids) != 1 {
		t.Errorf("the editor's coOrganizerIds %v", out["coOrganizerIds"])
	}

	// An Event with none answers none of the fields: its JSON is as before.
	plain := createEvent(t, svc, "YK", event.StatusPublished)
	raw, err := json.Marshal(svc.ProjectFor(&ykMember, plain))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "coOrganizer") {
		t.Fatalf("an Event without Co-organizers: %s", raw)
	}
}

func TestCoOrganizerPersonExpiry(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 12, 12, 10, 0, 0, 0, time.UTC)
	end := time.Date(2026, 12, 13, 18, 0, 0, 0, time.UTC)
	if at, ok := event.CoOrganizerPersonExpiry(event.Event{StartDate: &start, EndDate: &end}); !ok || !at.Equal(end.Add(30*24*time.Hour)) {
		t.Errorf("with an end: %v %v", at, ok)
	}
	if at, ok := event.CoOrganizerPersonExpiry(event.Event{StartDate: &start}); !ok || !at.Equal(start.Add(31*24*time.Hour)) {
		t.Errorf("with a start alone: %v %v", at, ok)
	}
	if _, ok := event.CoOrganizerPersonExpiry(event.Event{}); ok {
		t.Error("with no date")
	}
}

// Off (EVENT_CO_ORGANIZERS_WRITE unset), no write passes, not even
// event:manage's, and nobody may manage the list; reading it still works.
func TestOrganizersWritesOff(t *testing.T) {
	t.Parallel()
	svc := event.NewServiceWithOptions(event.NewMemoryStore(), authz.NewAuthorizer(authz.DefaultPolicy()), event.ServiceOptions{Teams: organizerTeams})
	ctx := context.Background()
	e := createEvent(t, svc, "WEBLAB", event.StatusPublished)
	_, err := svc.SetOrganizers(ctx, ykMember, e.ID, event.OrganizersInput{Teams: []event.OrganizerTeamInput{{Code: "ARTLAB"}}})
	if !errors.Is(err, event.ErrCoOrganizersDisabled) || !errors.Is(err, event.ErrForbidden) {
		t.Fatalf("PUT while off: %v", err)
	}
	got, err := svc.Organizers(ctx, ykMember, e.ID)
	if err != nil || got.CanManage || len(got.Teams) != 0 {
		t.Fatalf("GET while off: %+v %v", got, err)
	}
	if _, err := event.NewService(event.NewMemoryStore(), authz.NewAuthorizer(authz.DefaultPolicy())).SetOrganizers(ctx, ykMember, e.ID, event.OrganizersInput{}); !errors.Is(err, event.ErrCoOrganizersDisabled) {
		t.Fatalf("NewService's default: %v", err)
	}
}
