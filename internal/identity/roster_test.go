package identity_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

func seedTeam(t *testing.T, dir *identity.Memory) (memberID, leaderID uuid.UUID) {
	t.Helper()
	memberID = uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaa01")
	leaderID = uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaa02")
	dir.PutGroup(identity.Group{
		ID:   "g-weblab",
		Name: "WEBLAB",
		Path: "/UYELER/ARGE/WEBLAB",
		Attributes: map[string]string{
			"public_listing":  "true",
			"display_name_tr": "Web Laboratuvarı",
			"display_name_en": "Web Lab",
		},
	})
	dir.PutGroup(identity.Group{ID: "g-weblab-l", Name: "LIDERLER", Path: "/UYELER/ARGE/WEBLAB/LIDERLER"})
	dir.PutGroup(identity.Group{ID: "g-yk", Name: "YK", Path: "/UYELER/YK"})
	dir.PutUser(identity.Person{ID: memberID, Email: "member@example.com", FirstName: "Ada", LastName: "Member"})
	dir.PutUser(identity.Person{ID: leaderID, Email: "leader@example.com", FirstName: "Grace", LastName: "Leader"})
	ctx := context.Background()
	if err := dir.AddMember(ctx, "g-weblab", memberID); err != nil {
		t.Fatal(err)
	}
	if err := dir.AddMember(ctx, "g-weblab-l", leaderID); err != nil {
		t.Fatal(err)
	}
	return memberID, leaderID
}

func TestService_PublicMembersOmitsPIIAndMarksLeaders(t *testing.T) {
	t.Parallel()
	dir, _, svc := setup(t)
	_, leaderID := seedTeam(t, dir)

	roster, err := svc.PublicMembers(context.Background(), "WEBLAB")
	if err != nil {
		t.Fatal(err)
	}
	if roster.Team != "WEBLAB" || roster.Count != 2 {
		t.Fatalf("roster %+v", roster)
	}
	if roster.DisplayName.TR != "Web Laboratuvarı" {
		t.Fatalf("display %+v", roster.DisplayName)
	}

	raw, err := json.Marshal(roster)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "example.com") || strings.Contains(string(raw), leaderID.String()) {
		t.Fatalf("pii leaked: %s", raw)
	}

	var sawLeader, sawMember bool
	for _, m := range roster.Members {
		switch m.FirstName {
		case "Grace":
			sawLeader = true
			if !m.Leader {
				t.Fatal("leader subgroup member should be marked leader")
			}
		case "Ada":
			sawMember = true
			if m.Leader {
				t.Fatal("plain member should not be leader")
			}
		}
	}
	if !sawLeader || !sawMember {
		t.Fatalf("members %+v", roster.Members)
	}
}

func TestService_PublicMembersHidesPrivateAndMissing(t *testing.T) {
	t.Parallel()
	dir, _, svc := setup(t)
	seedTeam(t, dir)
	ctx := context.Background()

	if _, err := svc.PublicMembers(ctx, "YK"); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("YK should be hidden, got %v", err)
	}
	if _, err := svc.PublicMembers(ctx, "MISSING"); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("missing should 404, got %v", err)
	}
}

func TestService_ListPublicTeamsOmitsStructural(t *testing.T) {
	t.Parallel()
	dir, _, svc := setup(t)
	seedTeam(t, dir)

	teams, err := svc.ListPublicTeams(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(teams) != 1 || teams[0].Team != "WEBLAB" {
		t.Fatalf("teams %+v", teams)
	}
}

func TestService_PublicMembersFillsShadowProfile(t *testing.T) {
	t.Parallel()
	dir, store, svc := setup(t)
	memberID, _ := seedTeam(t, dir)
	_, _, err := user.NewService(store).Ensure(context.Background(), memberID, user.Profile{
		Email: "member@example.com", FirstName: "Ada", LastName: "Member",
	})
	if err != nil {
		t.Fatal(err)
	}
	pic := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	if _, err := user.NewService(store).SetProfilePicture(context.Background(), memberID, pic, "https://cdn.example.test/ada"); err != nil {
		t.Fatal(err)
	}
	linkedin := "https://linkedin.com/in/ada"
	if _, err := user.NewService(store).Patch(context.Background(), memberID, user.ProfilePatch{
		Linkedin: &linkedin, University: ptrStr("YTÜ"), Faculty: ptrStr("EE"), Department: ptrStr("CE"),
	}); err != nil {
		t.Fatal(err)
	}

	roster, err := svc.PublicMembers(context.Background(), "WEBLAB")
	if err != nil {
		t.Fatal(err)
	}
	var ada identity.PublicMember
	for _, m := range roster.Members {
		if m.FirstName == "Ada" {
			ada = m
		}
	}
	if ada.Linkedin != linkedin || ada.University != "YTÜ" || ada.Faculty != "EE" || ada.Department != "CE" || ada.ProfilePictureURL != "https://cdn.example.test/ada" {
		t.Fatalf("ada %+v", ada)
	}
	if ada.LastName != "Member" {
		t.Fatalf("name %+v", ada)
	}
}

func ptrStr(s string) *string { return &s }

func TestService_PublicLeadersOnlyLeaderSubgroups(t *testing.T) {
	t.Parallel()
	dir, _, svc := setup(t)
	_, leaderID := seedTeam(t, dir)

	roster, err := svc.PublicLeaders(context.Background(), "WEBLAB")
	if err != nil {
		t.Fatal(err)
	}
	if roster.Count != 1 || !roster.Members[0].Leader || roster.Members[0].FirstName != "Grace" {
		t.Fatalf("leaders %+v", roster.Members)
	}

	raw, err := json.Marshal(roster)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), leaderID.String()) || strings.Contains(string(raw), "example.com") {
		t.Fatalf("pii leaked: %s", raw)
	}
}
