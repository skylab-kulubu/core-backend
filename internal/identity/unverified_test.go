package identity_test

import (
	"context"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/user"
	"github.com/skylab-kulubu/core-backend/internal/ytu"
)

// The candidate list says who is YTÜ-linked, and warns only for a
// Privileged Group (grup-yonetimi 07).
func TestMemberCandidatesCarryTheYTUMarkAndTheWarning(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := identity.NewMemory()
	for _, g := range []identity.Group{
		{ID: "g-yk", Name: "YK", Path: "/UYELER/YK"},
		{ID: "g-weblab", Name: "WEBLAB", Path: "/UYELER/ARGE/WEBLAB"},
	} {
		dir.PutGroup(g)
	}
	users := user.NewMemoryStore()
	linked, unlinked := uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{linked, unlinked} {
		if _, _, err := users.Upsert(ctx, user.User{ID: id, Email: id.String() + "@example.test", FirstName: "Adaylar", LastName: id.String()[:4]}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := users.SetYTUProfile(ctx, linked, ytu.Profile{University: "YTÜ"}); err != nil {
		t.Fatal(err)
	}
	svc := identity.NewServiceWithOptions(dir, users, authz.NewAuthorizer(authz.DefaultPolicy()), identity.Options{})

	check := func(group string, wantWarned []uuid.UUID) {
		t.Helper()
		found, err := svc.MemberCandidates(ctx, actor(), group, "adaylar")
		if err != nil {
			t.Fatal(err)
		}
		if len(found) != 2 {
			t.Fatalf("found %+v", found)
		}
		for _, c := range found {
			if c.YTULinked != (c.ID == linked) {
				t.Fatalf("%s: ytuLinked %v", group, c)
			}
			warned := slices.Equal(c.Warnings, []string{identity.WarningNotYTUVerified})
			if warned != slices.Contains(wantWarned, c.ID) || c.Warnings == nil {
				t.Fatalf("%s: warnings %+v", group, c)
			}
		}
	}
	check("g-yk", []uuid.UUID{unlinked})
	check("g-weblab", nil)
}
