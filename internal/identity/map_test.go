package identity_test

import (
	"testing"

	"github.com/Nerzal/gocloak/v13"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/identity"
)

func TestGroupFromFlattensAttributesAndChildren(t *testing.T) {
	t.Parallel()
	childName := "WEBLAB"
	childPath := "/UYELER/ARGE/WEBLAB"
	childID := "c1"
	parentID := "p1"
	parentName := "ARGE"
	parentPath := "/UYELER/ARGE"
	attrs := map[string][]string{"public_listing": {"true"}}
	parent := &gocloak.Group{
		ID:         &parentID,
		Name:       &parentName,
		Path:       &parentPath,
		Attributes: &attrs,
		SubGroups: &[]gocloak.Group{{
			ID:   &childID,
			Name: &childName,
			Path: &childPath,
		}},
	}
	got := identity.FlattenGroupsForTest([]*gocloak.Group{parent})
	if len(got) != 2 {
		t.Fatalf("got %+v", got)
	}
	if got[0].Attributes["public_listing"] != "true" {
		t.Fatalf("attrs %+v", got[0].Attributes)
	}
	if got[1].Name != "WEBLAB" {
		t.Fatalf("child %+v", got[1])
	}
}

func TestPersonFromRequiresUUID(t *testing.T) {
	t.Parallel()
	id := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	ids := id.String()
	email := "ada@example.com"
	p, err := identity.PersonFromForTest(&gocloak.User{ID: &ids, Email: &email})
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != id || p.Email != email {
		t.Fatalf("got %+v", p)
	}
	bad := "not-uuid"
	if _, err := identity.PersonFromForTest(&gocloak.User{ID: &bad}); err == nil {
		t.Fatal("expected invalid")
	}
}

func TestClientRolesFromMappingsUsesClientId(t *testing.T) {
	t.Parallel()
	client := "cms-site"
	role := "cms:access"
	mapping := &gocloak.MappingsRepresentation{
		ClientMappings: map[string]*gocloak.ClientMappingsRepresentation{
			"cms-site": {
				Client:   &client,
				Mappings: &[]gocloak.Role{{Name: &role}},
			},
		},
	}
	got := identity.ClientRolesFromMappingsForTest(mapping)
	if len(got) != 1 || got[0].ClientID != "cms-site" || got[0].Role != "cms:access" {
		t.Fatalf("got %+v", got)
	}
}
