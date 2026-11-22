package event_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
)

// An Event's point and address (docs/event-location.md).

var locationLeader = authz.Principal{ID: "lider", Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}

func locationService() event.Service {
	return event.NewService(event.NewMemoryStore(), authz.NewAuthorizer(authz.DefaultPolicy()))
}

func degrees(f float64) *float64 { return &f }

func samePoint(e event.Event, lat, lng float64) bool {
	return e.LocationLat != nil && e.LocationLng != nil && *e.LocationLat == lat && *e.LocationLng == lng
}

// Create takes the point and the address; the point is kept to six
// decimals and the address trimmed.
func TestCreateKeepsThePointToSixDecimals(t *testing.T) {
	t.Parallel()
	created, err := locationService().Create(context.Background(), locationLeader, event.Event{
		Name: "Gecekodu", Location: "YTÜ Davutpaşa, EEF Konferans Salonu", OwnerTeam: "WEBLAB",
		LocationLat: degrees(41.02537649), LocationLng: degrees(28.8896481), LocationAddress: "  Davutpaşa Cd. No:127, Esenler  ",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !samePoint(created, 41.025376, 28.889648) || created.LocationAddress != "Davutpaşa Cd. No:127, Esenler" || !created.HasPoint() {
		t.Fatalf("created %v %v %q", created.LocationLat, created.LocationLng, created.LocationAddress)
	}
	none, err := locationService().Create(context.Background(), locationLeader, event.Event{Name: "Söyleşi", Location: "Online", OwnerTeam: "WEBLAB"})
	if err != nil || none.HasPoint() || none.LocationAddress != "" {
		t.Fatalf("without a point %+v %v", none, err)
	}
}

// A point off the globe, at 0, 0, or with one half missing, and an address
// too long or with control characters, are refused on create and on update.
func TestTheLocationIsChecked(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("a", event.LocationAddressMax+1)
	for name, in := range map[string]event.Event{
		"latitude alone":       {LocationLat: degrees(41)},
		"longitude alone":      {LocationLng: degrees(28)},
		"latitude above 90":    {LocationLat: degrees(90.5), LocationLng: degrees(28)},
		"latitude below -90":   {LocationLat: degrees(-91), LocationLng: degrees(28)},
		"longitude above 180":  {LocationLat: degrees(41), LocationLng: degrees(180.1)},
		"longitude below -180": {LocationLat: degrees(41), LocationLng: degrees(-181)},
		"zero, zero":           {LocationLat: degrees(0), LocationLng: degrees(0)},
		"address too long":     {LocationAddress: long},
		"address with a tab":   {LocationAddress: "Davutpaşa\tEsenler"},
	} {
		in.Name, in.Location, in.OwnerTeam = "Gecekodu", "YTÜ", "WEBLAB"
		if _, err := locationService().Create(context.Background(), locationLeader, in); !errors.Is(err, event.ErrLocationInvalid) || !errors.Is(err, event.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// The edges are places.
	edge, err := locationService().Create(context.Background(), locationLeader, event.Event{
		Name: "Kutup", Location: "Kuzey", OwnerTeam: "WEBLAB", LocationLat: degrees(90), LocationLng: degrees(-180),
		LocationAddress: strings.Repeat("ş", event.LocationAddressMax),
	})
	if err != nil || !samePoint(edge, 90, -180) {
		t.Fatalf("edge %+v %v", edge, err)
	}
}

// PATCH moves the point with both halves, clears it with null for both,
// and keeps it (and the address) when the body sends neither. One half
// alone is refused and changes nothing.
func TestPatchMovesAndClearsThePoint(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := locationService()
	created, err := svc.Create(ctx, locationLeader, event.Event{
		Name: "Gecekodu", Location: "YTÜ", OwnerTeam: "WEBLAB",
		LocationLat: degrees(41.025376), LocationLng: degrees(28.889648), LocationAddress: "Esenler",
	})
	if err != nil {
		t.Fatal(err)
	}
	kept, err := svc.Patch(ctx, locationLeader, created.ID, patchOf(t, `{"capacity":40}`))
	if err != nil || !samePoint(kept, 41.025376, 28.889648) || kept.LocationAddress != "Esenler" {
		t.Fatalf("kept %+v %v", kept, err)
	}
	if _, err := svc.Patch(ctx, locationLeader, created.ID, patchOf(t, `{"locationLat":41.05}`)); !errors.Is(err, event.ErrLocationInvalid) {
		t.Fatalf("latitude alone: %v", err)
	}
	if _, err := svc.Patch(ctx, locationLeader, created.ID, patchOf(t, `{"locationLat":null,"locationLng":28.9}`)); !errors.Is(err, event.ErrLocationInvalid) {
		t.Fatalf("half cleared: %v", err)
	}
	moved, err := svc.Patch(ctx, locationLeader, created.ID, patchOf(t, `{"locationLat":41.0431234567,"locationLng":29.0090001,"locationAddress":"Beşiktaş"}`))
	if err != nil || !samePoint(moved, 41.043123, 29.009) || moved.LocationAddress != "Beşiktaş" {
		t.Fatalf("moved %v %v %q %v", moved.LocationLat, moved.LocationLng, moved.LocationAddress, err)
	}
	cleared, err := svc.Patch(ctx, locationLeader, created.ID, patchOf(t, `{"locationLat":null,"locationLng":null,"locationAddress":null}`))
	if err != nil || cleared.HasPoint() || cleared.LocationLat != nil || cleared.LocationAddress != "" {
		t.Fatalf("cleared %+v %v", cleared, err)
	}
	if cleared.Location != "YTÜ" {
		t.Fatalf("the place's name went with the point: %q", cleared.Location)
	}
}

// The whole-Event update of a client from before the point keeps the
// stored point and address.
func TestUpdateWithoutAPointKeepsIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := locationService()
	created, err := svc.Create(ctx, locationLeader, event.Event{
		Name: "Gecekodu", Location: "YTÜ", OwnerTeam: "WEBLAB",
		LocationLat: degrees(41.025376), LocationLng: degrees(28.889648), LocationAddress: "Esenler",
	})
	if err != nil {
		t.Fatal(err)
	}
	updated, err := svc.Update(ctx, locationLeader, created.ID, event.Event{Name: "Gecekodu 2", Location: "YTÜ", OwnerTeam: "WEBLAB"})
	if err != nil || !samePoint(updated, 41.025376, 28.889648) || updated.LocationAddress != "Esenler" || updated.Name != "Gecekodu 2" {
		t.Fatalf("updated %+v %v", updated, err)
	}
}
