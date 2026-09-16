package competitor

import (
	"context"
	"errors"

	"github.com/google/uuid"
)

var (
	ErrNotFound  = errors.New("competitor: not found")
	ErrForbidden = errors.New("competitor: forbidden")
	ErrInvalid   = errors.New("competitor: invalid")
	ErrConflict  = errors.New("competitor: conflict")
)

type Store interface {
	List(ctx context.Context) ([]Competitor, error)
	Get(ctx context.Context, id uuid.UUID) (Competitor, error)
	Create(ctx context.Context, c Competitor) (Competitor, error)
	Update(ctx context.Context, c Competitor) (Competitor, error)
	Delete(ctx context.Context, id uuid.UUID) error
	ListByEvent(ctx context.Context, eventID uuid.UUID) ([]Competitor, error)
	ListByUser(ctx context.Context, userID uuid.UUID) ([]Competitor, error)
	ListByOwnerTeam(ctx context.Context, ownerTeam string) ([]Competitor, error)
	ExistsUserEvent(ctx context.Context, userID, eventID uuid.UUID) (bool, error)
	Winner(ctx context.Context, eventID uuid.UUID) (Competitor, error)
}
