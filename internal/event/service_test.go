package event_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
)

func setup(t *testing.T) (*event.MemoryStore, event.Service) {
	t.Helper()
	store := event.NewMemoryStore()
	return store, event.NewService(store, authz.NewAuthorizer(authz.DefaultPolicy()))
}

func TestService_CreateThenPublicGet(t *testing.T) {
	t.Parallel()
	store, svc := setup(t)
	ctx := context.Background()
	leader := authz.Principal{ID: "l", Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}

	created, err := svc.Create(ctx, leader, event.Event{
		Name:      "Hack",
		Location:  "YTÜ",
		OwnerTeam: "WEBLAB",
		Active:    true,
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := svc.Get(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "Hack" || got.OwnerTeam != "WEBLAB" {
		t.Fatalf("got %+v", got)
	}

	listed, err := svc.List(ctx, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 {
		t.Fatalf("listed %d", len(listed))
	}

	fromStore, err := store.Get(ctx, created.ID)
	if err != nil || fromStore.Name != "Hack" {
		t.Fatalf("store %+v %v", fromStore, err)
	}
}

func TestService_GeceKoduMemberCanCreateNotDelete(t *testing.T) {
	t.Parallel()
	_, svc := setup(t)
	ctx := context.Background()
	member := authz.Principal{ID: "m", Groups: []string{"/UYELER/ORGANIZASYON/GECEKODU"}}

	created, err := svc.Create(ctx, member, event.Event{Name: "GK", Location: "Kampüs", OwnerTeam: "GECEKODU"})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Delete(ctx, member, created.ID); !errors.Is(err, event.ErrForbidden) {
		t.Fatalf("delete: %v", err)
	}
}

func TestService_WrongTeamForbidden(t *testing.T) {
	t.Parallel()
	_, svc := setup(t)
	ctx := context.Background()
	p := authz.Principal{ID: "w", Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}

	_, err := svc.Create(ctx, p, event.Event{Name: "X", Location: "A", OwnerTeam: "SKYSEC"})
	if !errors.Is(err, event.ErrForbidden) {
		t.Fatalf("got %v", err)
	}
}

func TestService_RatioRequiresDecimal(t *testing.T) {
	t.Parallel()
	_, svc := setup(t)
	ctx := context.Background()
	leader := authz.Principal{ID: "l", Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
	_, err := svc.Create(ctx, leader, event.Event{
		Name: "ARTLAB", Location: "YTÜ", OwnerTeam: "WEBLAB", AttendanceRule: "ratio",
	})
	if !errors.Is(err, event.ErrInvalid) {
		t.Fatalf("got %v", err)
	}
	r := 0.75
	created, err := svc.Create(ctx, leader, event.Event{
		Name: "ARTLAB", Location: "YTÜ", OwnerTeam: "WEBLAB", AttendanceRule: "ratio", AttendanceRatio: &r,
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.AttendanceRule != "ratio" || created.AttendanceRatio == nil || *created.AttendanceRatio != 0.75 {
		t.Fatalf("created %+v", created)
	}
}

func TestService_PrivilegedCanDeleteAny(t *testing.T) {
	t.Parallel()
	_, svc := setup(t)
	ctx := context.Background()
	leader := authz.Principal{ID: "l", Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
	yk := authz.Principal{ID: "yk", Groups: []string{"/UYELER/YK"}}

	created, err := svc.Create(ctx, leader, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB"})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Delete(ctx, yk, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Get(ctx, created.ID); !errors.Is(err, event.ErrNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestService_UpdateOwnerTeamLeader(t *testing.T) {
	t.Parallel()
	_, svc := setup(t)
	ctx := context.Background()
	leader := authz.Principal{ID: "l", Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}

	created, err := svc.Create(ctx, leader, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB"})
	if err != nil {
		t.Fatal(err)
	}
	updated, err := svc.Update(ctx, leader, created.ID, event.Event{
		Name:      "Hack 2",
		Location:  "Davutpaşa",
		OwnerTeam: "WEBLAB",
		Active:    true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Name != "Hack 2" || updated.Location != "Davutpaşa" {
		t.Fatalf("updated %+v", updated)
	}
}

func TestService_CreateRequiresFields(t *testing.T) {
	t.Parallel()
	_, svc := setup(t)
	yk := authz.Principal{ID: "yk", Groups: []string{"/UYELER/YK"}}
	_, err := svc.Create(context.Background(), yk, event.Event{Name: "X"})
	if !errors.Is(err, event.ErrInvalid) {
		t.Fatalf("got %v", err)
	}
}

func TestService_PrivilegedCreatesEventWithoutOwnerTeam(t *testing.T) {
	t.Parallel()
	_, svc := setup(t)
	yk := authz.Principal{ID: "yk", Groups: []string{"/UYELER/YK"}}
	created, err := svc.Create(context.Background(), yk, event.Event{Name: "Seminer", Location: "YTÜ"})
	if err != nil {
		t.Fatal(err)
	}
	if created.OwnerTeam != "" {
		t.Fatalf("owner %q", created.OwnerTeam)
	}
}

func TestService_LeaderCannotCreateEventWithoutOwnerTeam(t *testing.T) {
	t.Parallel()
	_, svc := setup(t)
	leader := authz.Principal{ID: "l", Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
	_, err := svc.Create(context.Background(), leader, event.Event{Name: "Seminer", Location: "YTÜ"})
	if !errors.Is(err, event.ErrForbidden) {
		t.Fatalf("got %v", err)
	}
}

func TestService_CreateStoresCoverImageID(t *testing.T) {
	t.Parallel()
	_, svc := setup(t)
	ctx := context.Background()
	leader := authz.Principal{ID: "l", Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
	cover := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	created, err := svc.Create(ctx, leader, event.Event{
		Name:         "Hack",
		Location:     "YTÜ",
		OwnerTeam:    "WEBLAB",
		CoverImageID: &cover,
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.CoverImageID == nil || *created.CoverImageID != cover {
		t.Fatalf("cover %+v", created.CoverImageID)
	}
}
