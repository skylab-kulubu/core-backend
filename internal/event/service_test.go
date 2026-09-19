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

func TestService_LeaderCannotTransferEventToOtherTeam(t *testing.T) {
	t.Parallel()
	_, svc := setup(t)
	ctx := context.Background()
	leader := authz.Principal{ID: "l", Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}

	created, err := svc.Create(ctx, leader, event.Event{
		Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.Update(ctx, leader, created.ID, event.Event{
		Name: "Hack", Location: "YTÜ", OwnerTeam: "SKYSEC",
	})
	if !errors.Is(err, event.ErrForbidden) {
		t.Fatalf("transfer: %v", err)
	}
	stored, err := svc.Get(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.OwnerTeam != "WEBLAB" {
		t.Fatalf("owner %q", stored.OwnerTeam)
	}
}

func TestService_PrivilegedCanTransferEventToOtherTeam(t *testing.T) {
	t.Parallel()
	_, svc := setup(t)
	ctx := context.Background()
	yk := authz.Principal{ID: "yk", Groups: []string{"/UYELER/YK"}}

	created, err := svc.Create(ctx, yk, event.Event{
		Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB",
	})
	if err != nil {
		t.Fatal(err)
	}
	updated, err := svc.Update(ctx, yk, created.ID, event.Event{
		Name: "Hack", Location: "YTÜ", OwnerTeam: "SKYSEC",
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.OwnerTeam != "SKYSEC" {
		t.Fatalf("owner %q", updated.OwnerTeam)
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

func TestService_PrivilegedAssignsDoorStaff(t *testing.T) {
	t.Parallel()
	_, svc := setup(t)
	ctx := context.Background()
	yk := authz.Principal{ID: "yk", Groups: []string{"/UYELER/YK"}}
	staff := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	created, err := svc.Create(ctx, yk, event.Event{
		Name:         "Hack",
		Location:     "YTÜ",
		OwnerTeam:    "WEBLAB",
		DoorStaffIDs: []uuid.UUID{staff},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(created.DoorStaffIDs) != 1 || created.DoorStaffIDs[0] != staff {
		t.Fatalf("created staff %+v", created.DoorStaffIDs)
	}

	other := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	updated, err := svc.Update(ctx, yk, created.ID, event.Event{
		Name:         "Hack",
		Location:     "YTÜ",
		OwnerTeam:    "WEBLAB",
		DoorStaffIDs: []uuid.UUID{other},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.DoorStaffIDs) != 1 || updated.DoorStaffIDs[0] != other {
		t.Fatalf("updated staff %+v", updated.DoorStaffIDs)
	}

	cleared, err := svc.Update(ctx, yk, created.ID, event.Event{
		Name:         "Hack",
		Location:     "YTÜ",
		OwnerTeam:    "WEBLAB",
		DoorStaffIDs: []uuid.UUID{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cleared.DoorStaffIDs) != 0 {
		t.Fatalf("cleared staff %+v", cleared.DoorStaffIDs)
	}
}

func TestService_LeaderCannotAssignDoorStaff(t *testing.T) {
	t.Parallel()
	_, svc := setup(t)
	ctx := context.Background()
	yk := authz.Principal{ID: "yk", Groups: []string{"/UYELER/YK"}}
	leader := authz.Principal{ID: "l", Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
	staff := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	created, err := svc.Create(ctx, yk, event.Event{
		Name:         "Hack",
		Location:     "YTÜ",
		OwnerTeam:    "WEBLAB",
		DoorStaffIDs: []uuid.UUID{staff},
	})
	if err != nil {
		t.Fatal(err)
	}

	updated, err := svc.Update(ctx, leader, created.ID, event.Event{
		Name:         "Hack 2",
		Location:     "Davutpaşa",
		OwnerTeam:    "WEBLAB",
		DoorStaffIDs: []uuid.UUID{uuid.MustParse("cccccccc-cccc-cccc-cccc-cccccccccccc")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Name != "Hack 2" {
		t.Fatalf("name %+v", updated)
	}
	if len(updated.DoorStaffIDs) != 1 || updated.DoorStaffIDs[0] != staff {
		t.Fatalf("leader overwrote staff %+v", updated.DoorStaffIDs)
	}

	fromLeader, err := svc.Create(ctx, leader, event.Event{
		Name:         "Mini",
		Location:     "YTÜ",
		OwnerTeam:    "WEBLAB",
		DoorStaffIDs: []uuid.UUID{staff},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(fromLeader.DoorStaffIDs) != 0 {
		t.Fatalf("leader created staff %+v", fromLeader.DoorStaffIDs)
	}
}

func TestService_PublicMediaURLsDoNotRewriteStore(t *testing.T) {
	t.Parallel()
	store := event.NewMemoryStore()
	svc := event.NewService(store, authz.NewAuthorizer(authz.DefaultPolicy()), "https://cdn.example.test")
	ctx := context.Background()
	cover := "images/b0db8eb9-5914-4db7-a39a-cabd6bf47faa"
	gallery := "images/7f863471-c2b6-45f4-912d-80f97daf25f4"
	href := "https://cdn.yildizskylab.com/images/abc"
	imgID := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	absID := uuid.MustParse("cccccccc-cccc-cccc-cccc-cccccccccccc")
	slashID := uuid.MustParse("dddddddd-dddd-dddd-dddd-dddddddddddd")
	created, err := store.Create(ctx, event.Event{
		Name:          "Hack",
		Location:      "YTÜ",
		OwnerTeam:     "WEBLAB",
		CoverImageURL: cover,
		CoverColors:   []string{"#3c82be", "#8a642f"},
		Images: []event.GalleryImage{
			{ID: imgID, URL: gallery},
			{ID: absID, URL: href},
			{ID: slashID, URL: "/images/slash"},
		},
		ImageURLs: []string{gallery, href, "/images/slash"},
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := svc.Get(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.CoverImageURL != "https://cdn.example.test/images/b0db8eb9-5914-4db7-a39a-cabd6bf47faa" {
		t.Fatalf("cover %s", got.CoverImageURL)
	}
	if len(got.Images) != 3 || got.Images[0].URL != "https://cdn.example.test/images/7f863471-c2b6-45f4-912d-80f97daf25f4" {
		t.Fatalf("gallery %+v", got.Images)
	}
	if got.Images[1].URL != href {
		t.Fatalf("absolute %s", got.Images[1].URL)
	}
	if got.Images[2].URL != "https://cdn.example.test/images/slash" {
		t.Fatalf("slash %s", got.Images[2].URL)
	}
	if len(got.ImageURLs) != 3 || got.ImageURLs[0] != got.Images[0].URL || got.ImageURLs[1] != href || got.ImageURLs[2] != got.Images[2].URL {
		t.Fatalf("imageUrls %+v", got.ImageURLs)
	}

	stored, err := store.Get(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.CoverImageURL != cover || stored.Images[0].URL != gallery || stored.ImageURLs[0] != gallery {
		t.Fatalf("store mutated %+v", stored)
	}

	res := stored.Resource()
	if res.CoverImageURL != "https://cdn.yildizskylab.com/images/b0db8eb9-5914-4db7-a39a-cabd6bf47faa" {
		t.Fatalf("resource %s", res.CoverImageURL)
	}
	if len(res.CoverColors) != 2 || res.CoverColors[0] != "#3c82be" {
		t.Fatalf("resource colors %#v", res.CoverColors)
	}
}
