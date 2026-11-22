package event_test

import (
	"context"
	"errors"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
)

// placesFixture is a store with Events of two teams at a few places: the
// same place twice (its address changed), a draft, an archived Event, and
// an Event with no point.
func placesFixture(t *testing.T, store event.Store) {
	t.Helper()
	ctx := context.Background()
	for _, e := range []event.Event{
		{Name: "Gecekodu", Location: "YTÜ Davutpaşa, EEF Konferans Salonu", OwnerTeam: "WEBLAB", StartDate: stamp("2026-03-14T19:00:00+03:00"),
			LocationLat: degrees(41.025376), LocationLng: degrees(28.889648), LocationAddress: "Esenler"},
		{Name: "Gecekodu 2", Location: "YTÜ Davutpaşa, EEF Konferans Salonu", OwnerTeam: "WEBLAB", StartDate: stamp("2026-10-14T19:00:00+03:00"),
			LocationLat: degrees(41.025376), LocationLng: degrees(28.889648), LocationAddress: "Davutpaşa Cd. No:127, Esenler"},
		{Name: "Söyleşi", Location: "Yıldız Kampüsü, Oditoryum", OwnerTeam: "WEBLAB", StartDate: stamp("2026-05-01T14:00:00+03:00"),
			LocationLat: degrees(41.050835), LocationLng: degrees(29.010364)},
		{Name: "Taslak", Location: "Taslak Salonu", OwnerTeam: "WEBLAB", Status: event.StatusDraft, StartDate: stamp("2026-12-01T10:00:00+03:00"),
			LocationLat: degrees(41.1), LocationLng: degrees(29.1)},
		{Name: "Online", Location: "Online", OwnerTeam: "WEBLAB", StartDate: stamp("2026-11-01T10:00:00+03:00")},
		{Name: "Sanat", Location: "ARTLAB Atölyesi", OwnerTeam: "ARTLAB", StartDate: stamp("2026-09-01T10:00:00+03:00"),
			LocationLat: degrees(41.03), LocationLng: degrees(28.9)},
	} {
		if e.Status == "" {
			e.Status = event.StatusPublished
		}
		if _, err := store.Create(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	archived, err := store.Create(ctx, event.Event{Name: "Eski", Location: "Arşiv Salonu", OwnerTeam: "WEBLAB", Status: event.StatusPublished,
		LocationLat: degrees(41.2), LocationLng: degrees(29.2)})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Archive(ctx, archived.ID, nil); err != nil {
		t.Fatal(err)
	}
}

func placeNames(places []event.Place) []string {
	out := make([]string, len(places))
	for i, p := range places {
		out[i] = p.Name
	}
	return out
}

// A team's places: one per name and point, the latest used first, with
// the address of the latest Event and the number of Events; the team's
// drafts count, archived Events and Events with no point do not.
func TestPlacesOfATeam(t *testing.T) {
	t.Parallel()
	store := event.NewMemoryStore()
	placesFixture(t, store)
	svc := event.NewService(store, authz.NewAuthorizer(authz.DefaultPolicy()))
	places, err := svc.Places(context.Background(), locationLeader, "WEBLAB", 0)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Taslak Salonu", "YTÜ Davutpaşa, EEF Konferans Salonu", "Yıldız Kampüsü, Oditoryum"}
	if got := placeNames(places); len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("places %v", got)
	}
	davutpasa := places[1]
	if davutpasa.Uses != 2 || davutpasa.Address != "Davutpaşa Cd. No:127, Esenler" || davutpasa.Lat != 41.025376 || davutpasa.Lng != 28.889648 ||
		!davutpasa.LastUsedAt.Equal(*stamp("2026-10-14T19:00:00+03:00")) {
		t.Fatalf("Davutpaşa %+v", davutpasa)
	}
	if limited, err := svc.Places(context.Background(), locationLeader, "WEBLAB", 1); err != nil || len(limited) != 1 {
		t.Fatalf("limit 1: %v %v", placeNames(limited), err)
	}
}

// The club's places are for event:manage, from every team's Events but no
// draft; a team's for who may create Events there; anyone else, and a
// limit out of range, is refused.
func TestPlacesWhoMayAsk(t *testing.T) {
	t.Parallel()
	store := event.NewMemoryStore()
	placesFixture(t, store)
	svc := event.NewService(store, authz.NewAuthorizer(authz.DefaultPolicy()))
	ctx := context.Background()
	yk := authz.Principal{ID: "yk", Groups: []string{"/UYELER/YK"}}
	club, err := svc.Places(ctx, yk, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := placeNames(club); len(got) != 3 || got[0] != "YTÜ Davutpaşa, EEF Konferans Salonu" || got[1] != "ARTLAB Atölyesi" || got[2] != "Yıldız Kampüsü, Oditoryum" {
		t.Fatalf("club places %v", got)
	}
	member := authz.Principal{ID: "uye", Groups: []string{"/UYELER/ARGE/WEBLAB"}}
	for name, call := range map[string]func() error{
		"a leader for the club":       func() error { _, err := svc.Places(ctx, locationLeader, "", 0); return err },
		"a leader for another team":   func() error { _, err := svc.Places(ctx, locationLeader, "ARTLAB", 0); return err },
		"a member for their own team": func() error { _, err := svc.Places(ctx, member, "WEBLAB", 0); return err },
	} {
		if err := call(); !errors.Is(err, event.ErrForbidden) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for _, limit := range []int{-1, event.PlacesMax + 1} {
		if _, err := svc.Places(ctx, yk, "", limit); !errors.Is(err, event.ErrInvalid) {
			t.Errorf("limit %d: %v", limit, err)
		}
	}
}
