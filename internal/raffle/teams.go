package raffle

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/identity"
)

// Member is a person an exclusion leaves out: their account and addresses.
type Member struct {
	ID     uuid.UUID
	Emails []string
}

// Teams reads who is in a Team, its sub-teams included.
type Teams interface {
	TeamMembers(ctx context.Context, code string) ([]Member, error)
}

// maxTeamGroups bounds the Groups one Team's reading walks.
const maxTeamGroups = 200

// ErrDirectory is the directory not answering: the exclusions cannot be
// read, so nothing is previewed or drawn (503 raffle_directory_unavailable).
var ErrDirectory = errors.New("raffle: directory unavailable")

// DirectoryTeams reads Teams from the identity directory (Keycloak): the
// Team's Group and every Group under it, their direct members each.
type DirectoryTeams struct {
	Dir identity.Directory
}

func (d DirectoryTeams) TeamMembers(ctx context.Context, code string) ([]Member, error) {
	if d.Dir == nil || code == "" {
		return nil, nil
	}
	root, err := d.Dir.GetGroup(ctx, code)
	if errors.Is(err, identity.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.Join(ErrDirectory, err)
	}
	queue := []string{root.ID}
	seen := map[string]bool{root.ID: true}
	people := map[uuid.UUID]bool{}
	var out []Member
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		members, err := d.Dir.Members(ctx, id)
		if err != nil {
			return nil, errors.Join(ErrDirectory, err)
		}
		for _, p := range members {
			if people[p.ID] {
				continue
			}
			people[p.ID] = true
			out = append(out, Member{ID: p.ID, Emails: []string{p.Email, p.SchoolEmail}})
		}
		subs, err := d.Dir.Subgroups(ctx, id)
		if err != nil {
			return nil, errors.Join(ErrDirectory, err)
		}
		for _, g := range subs {
			if seen[g.ID] || len(seen) >= maxTeamGroups {
				continue
			}
			seen[g.ID] = true
			queue = append(queue, g.ID)
		}
	}
	return out, nil
}
