package event

import (
	"context"
	"time"

	"github.com/skylab-kulubu/core-backend/internal/authz"
)

// Place is a place Events were held at, as the panel offers it when an
// organizer types a new Event's place (docs/event-location.md, "Places
// used before"): its name, address and point, when it was last used and
// how many Events used it.
type Place struct {
	Name       string    `json:"name"`
	Address    string    `json:"address,omitempty"`
	Lat        float64   `json:"lat"`
	Lng        float64   `json:"lng"`
	LastUsedAt time.Time `json:"lastUsedAt"`
	Uses       int       `json:"uses"`
}

// PlacesLimit and PlacesMax are how many places a list answers by default
// and at most.
const (
	PlacesLimit = 20
	PlacesMax   = 50
)

// Places are the places the current Events with a point were held at, one
// per name and point (the address of its latest Event), the latest used
// first: the Owner team's Events, drafts included, for who may create
// Events in that team; or every team's published, cancelled and ended
// Events (no draft) for who holds event:manage. ErrInvalid for a limit
// outside 1–PlacesMax, ErrForbidden for anyone else.
func (s *service) Places(ctx context.Context, p authz.Principal, ownerTeam string, limit int) ([]Place, error) {
	if limit == 0 {
		limit = PlacesLimit
	}
	if limit < 0 || limit > PlacesMax {
		return nil, ErrInvalid
	}
	drafts := ownerTeam != ""
	switch {
	case ownerTeam != "" && s.authz.Allow(p, resource(ownerTeam), authz.Create):
	case ownerTeam == "" && s.authz.Permitted(p, authz.RoleEventManage):
	default:
		return nil, ErrForbidden
	}
	return s.store.ListPlaces(ctx, ownerTeam, drafts, limit)
}
