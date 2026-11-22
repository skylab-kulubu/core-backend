package event_test

import (
	"context"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
)

// The database lists the places as the memory store does: one per name and
// point, the latest first, drafts only when asked, archived Events never.
func TestPostgresListsPlaces(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := event.NewPostgresStore(pool)
	placesFixture(t, store)
	team, err := store.ListPlaces(ctx, "WEBLAB", true, 20)
	if err != nil {
		t.Fatal(err)
	}
	if got := placeNames(team); len(got) != 3 || got[0] != "Taslak Salonu" || got[1] != "YTÜ Davutpaşa, EEF Konferans Salonu" || got[2] != "Yıldız Kampüsü, Oditoryum" {
		t.Fatalf("team places %v", got)
	}
	if d := team[1]; d.Uses != 2 || d.Address != "Davutpaşa Cd. No:127, Esenler" || !d.LastUsedAt.Equal(*stamp("2026-10-14T19:00:00+03:00")) {
		t.Fatalf("Davutpaşa %+v", d)
	}
	club, err := store.ListPlaces(ctx, "", false, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got := placeNames(club); len(got) != 2 || got[0] != "YTÜ Davutpaşa, EEF Konferans Salonu" || got[1] != "ARTLAB Atölyesi" {
		t.Fatalf("club places %v", got)
	}
}
