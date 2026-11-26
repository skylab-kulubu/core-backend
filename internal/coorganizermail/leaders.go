package coorganizermail

import (
	"context"
	"errors"
	"slices"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/identity"
)

// LeaderDirectory is what DirectoryLeaders reads (identity.Directory).
type LeaderDirectory interface {
	GetGroup(ctx context.Context, idOrPath string) (identity.Group, error)
	Members(ctx context.Context, groupID string) ([]identity.Person, error)
}

// DirectoryLeaders reads a team's Leaders from the directory (Keycloak) as
// they are now: the enabled members of its own Leader groups (LIDERLER,
// KOORDINATORLER), not of a Team above it.
type DirectoryLeaders struct {
	Teams     authz.TeamTree
	Directory LeaderDirectory
	// LeaderSubgroups are the Leader groups' names (authz.Policy).
	LeaderSubgroups []string
}

func (d DirectoryLeaders) LeaderIDs(ctx context.Context, teamCode string) ([]uuid.UUID, error) {
	path, ok := d.Teams.TeamPath(teamCode)
	if !ok {
		return nil, nil
	}
	var out []uuid.UUID
	for _, sub := range d.LeaderSubgroups {
		g, err := d.Directory.GetGroup(ctx, path+"/"+sub)
		if errors.Is(err, identity.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		members, err := d.Directory.Members(ctx, g.ID)
		if err != nil {
			return nil, err
		}
		for _, m := range members {
			if m.Enabled && !slices.Contains(out, m.ID) {
				out = append(out, m.ID)
			}
		}
	}
	return out, nil
}
