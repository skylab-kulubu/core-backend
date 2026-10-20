package season

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/optional"
)

// Patch is the body of PUT and PATCH /v1/seasons/{id}: a field left out
// keeps the Season's value, a null date clears it (ticket 13). The name may
// not be emptied, nor active set null: active is the Season's own flag ("the
// current season"), set by hand, so it changes only when the body sends it.
type Patch struct {
	Name      optional.Field[string]     `json:"name"`
	StartDate optional.Field[*time.Time] `json:"startDate"`
	EndDate   optional.Field[*time.Time] `json:"endDate"`
	Active    optional.Field[bool]       `json:"active"`
}

func (p Patch) apply(existing Season) (Season, error) {
	if p.Name.Set && strings.TrimSpace(p.Name.Value) == "" {
		return Season{}, ErrInvalid
	}
	if p.Active.Null {
		return Season{}, ErrInvalid
	}
	out := existing
	out.Name = p.Name.Or(existing.Name)
	out.StartDate = p.StartDate.Or(existing.StartDate)
	out.EndDate = p.EndDate.Or(existing.EndDate)
	out.Active = p.Active.Or(existing.Active)
	return out, nil
}

// Patch writes the fields patch sent and keeps every other.
func (s *service) Patch(ctx context.Context, p authz.Principal, id uuid.UUID, patch Patch) (Season, error) {
	existing, err := s.store.Get(ctx, id)
	if err != nil {
		return Season{}, err
	}
	if !s.authz.Allow(p, authz.Resource{Type: authz.TypeSeason}, authz.Update) {
		return Season{}, ErrForbidden
	}
	in, err := patch.apply(existing)
	if err != nil {
		return Season{}, err
	}
	return s.store.Update(ctx, in)
}
