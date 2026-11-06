package identity_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/identity"
)

// The team page notice reads whom the public rosters list before consents:
// a listed team's members through its subgroups, a team whose leaders alone
// are public its leaders, the Board's own roster marked; nobody erased,
// no Group that is not public.
func TestTeamPageListingsAreThePublicRosters(t *testing.T) {
	t.Parallel()
	f := newTeamPageFixture(t)
	ctx := context.Background()
	f.dir.PutGroup(identity.Group{ID: "g-artlab", Name: "ARTLAB", Path: "/UYELER/ORGANIZASYON/ARTLAB", Attributes: map[string]string{"public_leaders": "true"}})
	f.dir.PutGroup(identity.Group{ID: "g-artlab-l", Name: "LIDERLER", Path: "/UYELER/ORGANIZASYON/ARTLAB/LIDERLER"})
	f.dir.PutGroup(identity.Group{ID: "g-hidden", Name: "GIZLI", Path: "/UYELER/ARGE/GIZLI"})
	artMember, artLeader, hidden := uuid.New(), uuid.New(), uuid.New()
	for id, group := range map[uuid.UUID]string{artMember: "g-artlab", artLeader: "g-artlab-l", hidden: "g-hidden"} {
		f.dir.PutUser(identity.Person{ID: id, Email: id.String() + "@example.com", FirstName: "X", LastName: "Y"})
		if err := f.dir.AddMember(ctx, group, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.store.RequestDeletion(ctx, f.leader, nil); err != nil {
		t.Fatal(err)
	}

	listings, err := identity.TeamPageListings(ctx, f.dir, f.store, authz.DefaultPrivilegedRoots())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]identity.TeamPageListing{}
	var order []string
	for _, l := range listings {
		got[l.Team.Path] = l
		order = append(order, l.Team.Path)
	}
	if !slices.Equal(order, []string{"/UYELER/ARGE/WEBLAB", "/UYELER/ORGANIZASYON/ARTLAB", "/UYELER/YK"}) {
		t.Fatalf("listings %v", order)
	}
	weblab := got["/UYELER/ARGE/WEBLAB"]
	// Grace leads WEBLAB but is being erased: the roster leaves her out.
	if weblab.Board || !sameIDs(weblab.People, f.member, f.board) {
		t.Fatalf("WEBLAB %+v", weblab)
	}
	if art := got["/UYELER/ORGANIZASYON/ARTLAB"]; art.Board || !sameIDs(art.People, artLeader) {
		t.Fatalf("ARTLAB leaders %+v", art)
	}
	if yk := got["/UYELER/YK"]; !yk.Board || !sameIDs(yk.People, f.board) {
		t.Fatalf("YK %+v", yk)
	}
}

func sameIDs(got []uuid.UUID, want ...uuid.UUID) bool {
	a, b := slices.Clone(got), slices.Clone(want)
	cmp := func(x, y uuid.UUID) int { return slices.Compare(x[:], y[:]) }
	slices.SortFunc(a, cmp)
	slices.SortFunc(b, cmp)
	return slices.Equal(a, b)
}

// incompleteDirectory is a directory whose Group list missed a branch.
type incompleteDirectory struct{ *identity.Memory }

func (incompleteDirectory) ListGroupsComplete(context.Context) ([]identity.Group, error) {
	return nil, identity.ErrIncompleteGroups
}

// A list that missed a branch is an error: its members would be hidden
// untold.
func TestTeamPageListingsRefuseAnIncompleteGroupList(t *testing.T) {
	t.Parallel()
	f := newTeamPageFixture(t)
	_, err := identity.TeamPageListings(context.Background(), incompleteDirectory{f.dir}, f.store, authz.DefaultPrivilegedRoots())
	if !errors.Is(err, identity.ErrIncompleteGroups) {
		t.Fatalf("err %v", err)
	}
}
