package identity

import (
	"context"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// TeamPageListing is one public roster (GET /v1/teams/{team}/members of a
// Group with public_listing=true, or /leaders of one with
// public_leaders=true) and everyone it lists before the team page rule
// looks at consents: the people the transition still shows in full when
// they have not decided (docs/team-page-consent.md). The team page notice
// (ekip-yetki-devri 21) reads them.
type TeamPageListing struct {
	Team Group
	// Board is the Board's own roster: a person without the consent stays
	// on it by first and last name.
	Board bool
	// People are the listed, each once: no erased or deletion-pending
	// person, as the roster leaves them out.
	People []uuid.UUID
}

// TeamPageListings reads every public roster of the realm the way the
// rosters do: a listed team's members through every Group under it, a team
// whose leaders alone are public its LIDERLER and KOORDINATORLER. It reads
// no consent. The listings are in path order. A directory that can tell
// a Group list missing a branch (Keycloak's ListGroupsComplete) is read
// with it, and such a list is ErrIncompleteGroups.
func TeamPageListings(ctx context.Context, dir Directory, users user.Store, privilegedRoots []string) ([]TeamPageListing, error) {
	s := &service{dir: dir, users: users, privilegedRoots: privilegedRoots}
	// A list missing a branch (ErrIncompleteGroups: Keycloak restarting, a
	// Group moved mid-read) would leave that branch's members untold while
	// the transition hides them: it is an error, not a shorter list.
	var groups []Group
	var err error
	if complete, ok := dir.(interface {
		ListGroupsComplete(ctx context.Context) ([]Group, error)
	}); ok {
		groups, err = complete.ListGroupsComplete(ctx)
	} else {
		groups, err = dir.ListGroups(ctx)
	}
	if err != nil {
		return nil, err
	}
	slices.SortFunc(groups, func(a, b Group) int { return strings.Compare(a.Path, b.Path) })
	var out []TeamPageListing
	for _, g := range groups {
		var ids []uuid.UUID
		switch {
		case attrTrue(g.Attributes, "public_listing"):
			people, err := s.collectMembers(ctx, g, true)
			if err != nil {
				return nil, err
			}
			for _, p := range people {
				ids = append(ids, p.ID)
			}
		case attrTrue(g.Attributes, "public_leaders"):
			if ids, err = s.leaders(ctx, g); err != nil {
				return nil, err
			}
		default:
			continue
		}
		accounts, err := users.Accounts(ctx, ids)
		if err != nil {
			return nil, err
		}
		listing := TeamPageListing{Team: g, Board: s.boardRoster(g)}
		for _, id := range ids {
			if id != user.DeletedSubject && !accounts[id].Blocked {
				listing.People = append(listing.People, id)
			}
		}
		out = append(out, listing)
	}
	return out, nil
}
