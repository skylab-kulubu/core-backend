package event_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/organizer"
)

// An Event of the Board (YK), DK or ADMIN is organized by the club: its
// answers say the club's name in organizerName, and keep ownerTeam (Yusuf,
// 2026-11-23). Any other team's Event says its code.
func TestService_OrganizerName(t *testing.T) {
	t.Parallel()
	_, svc := setup(t)
	ctx := context.Background()
	admin := authz.Principal{ID: "a", Groups: []string{"/ADMIN"}}

	for team, want := range map[string]string{
		"YK":     organizer.ClubName,
		"ADMIN":  organizer.ClubName,
		"DK":     organizer.ClubName,
		"WEBLAB": "WEBLAB",
	} {
		created, err := svc.Create(ctx, admin, event.Event{Name: "Etkinlik " + team, Location: "YTÜ", OwnerTeam: team, Active: true})
		if err != nil {
			t.Fatal(team, err)
		}
		if created.OrganizerName != want || created.OwnerTeam != team {
			t.Fatalf("%s: created organizer %q owner %q", team, created.OrganizerName, created.OwnerTeam)
		}
		got, err := svc.Get(ctx, nil, created.ID)
		if err != nil {
			t.Fatal(team, err)
		}
		if got.OrganizerName != want || got.OwnerTeam != team {
			t.Fatalf("%s: public get organizer %q owner %q", team, got.OrganizerName, got.OwnerTeam)
		}
		body, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), `"organizerName":"`+want+`"`) || !strings.Contains(string(body), `"ownerTeam":"`+team+`"`) {
			t.Fatalf("%s: JSON %s", team, body)
		}
	}

	list, err := svc.List(ctx, nil, "", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 4 {
		t.Fatalf("list %d", len(list))
	}
	for _, e := range list {
		if e.OrganizerName != organizer.Name(e.OwnerTeam) {
			t.Fatalf("list %s organizer %q", e.OwnerTeam, e.OrganizerName)
		}
	}
}

func TestResource_OrganizerName(t *testing.T) {
	t.Parallel()
	for team, want := range map[string]string{"YK": organizer.ClubName, "": organizer.ClubName, "GECEKODU": "GECEKODU"} {
		res := event.Event{Name: "E", OwnerTeam: team}.Resource()
		if res.OrganizerName != want || res.OwnerTeam != team {
			t.Fatalf("%q: resource organizer %q owner %q", team, res.OrganizerName, res.OwnerTeam)
		}
	}
}
