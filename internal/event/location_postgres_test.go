package event_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
)

// The database stores the point and the address, moves and clears them,
// copies them, and its check refuses half a point whoever writes it.
func TestPostgresStoresTheLocation(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := event.NewPostgresStore(pool)
	svc := event.NewService(store, authz.NewAuthorizer(authz.DefaultPolicy()))
	yk := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/YK"}}
	created, err := svc.Create(ctx, yk, event.Event{
		Name: "Gecekodu", Location: "YTÜ Davutpaşa, EEF Konferans Salonu", OwnerTeam: "WEBLAB",
		StartDate:   stamp("2026-12-12T09:30:00+03:00"),
		LocationLat: degrees(41.02537649), LocationLng: degrees(28.8896481), LocationAddress: "Davutpaşa Cd. No:127, Esenler",
	})
	if err != nil {
		t.Fatal(err)
	}
	read, err := store.Get(ctx, created.ID)
	if err != nil || !samePoint(read, 41.025376, 28.889648) || read.LocationAddress != "Davutpaşa Cd. No:127, Esenler" {
		t.Fatalf("read %v %v %q %v", read.LocationLat, read.LocationLng, read.LocationAddress, err)
	}
	listed, err := store.List(ctx, "WEBLAB", false)
	if err != nil || len(listed) != 1 || !samePoint(listed[0], 41.025376, 28.889648) {
		t.Fatalf("listed %+v %v", listed, err)
	}

	moved, err := svc.Patch(ctx, yk, created.ID, patchOf(t, `{"locationLat":41.043123,"locationLng":29.009}`))
	if err != nil || !samePoint(moved, 41.043123, 29.009) || moved.LocationAddress != "Davutpaşa Cd. No:127, Esenler" {
		t.Fatalf("moved %+v %v", moved, err)
	}

	copied, _, err := svc.Copy(ctx, yk, created.ID, event.CopyInput{ID: uuid.New(), Name: "Gecekodu 2027", StartDate: stamp("2027-12-11T09:30:00+03:00")})
	if err != nil || !samePoint(copied.Event, 41.043123, 29.009) || copied.Event.LocationAddress != "Davutpaşa Cd. No:127, Esenler" {
		t.Fatalf("copied %+v %v", copied.Event, err)
	}

	cleared, err := svc.Patch(ctx, yk, created.ID, patchOf(t, `{"locationLat":null,"locationLng":null,"locationAddress":""}`))
	if err != nil || cleared.HasPoint() || cleared.LocationAddress != "" {
		t.Fatalf("cleared %+v %v", cleared, err)
	}

	// The check, for a writer that is not core.
	for name, sql := range map[string]string{
		"latitude alone":   `UPDATE events SET location_lat = 41 WHERE id = $1`,
		"off the globe":    `UPDATE events SET location_lat = 91, location_lng = 28 WHERE id = $1`,
		"zero, zero":       `UPDATE events SET location_lat = 0, location_lng = 0 WHERE id = $1`,
		"address too long": `UPDATE events SET location_address = repeat('a', 301) WHERE id = $1`,
	} {
		if _, err := pool.Exec(ctx, sql, created.ID); err == nil {
			t.Errorf("%s: the database took it", name)
		}
	}
}
