package event

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/lifecycle"
)

var (
	ErrNotFound  = errors.New("event: not found")
	ErrForbidden = errors.New("event: forbidden")
	ErrInvalid   = errors.New("event: invalid")
)

type Store interface {
	List(ctx context.Context, ownerTeam string, activeOnly bool) ([]Event, error)
	ListLifecycle(ctx context.Context, ownerTeam string, visibility lifecycle.Visibility) ([]Event, error)
	Get(ctx context.Context, id uuid.UUID) (Event, error)
	GetIncludingArchived(ctx context.Context, id uuid.UUID) (Event, error)
	Create(ctx context.Context, e Event) (Event, error)
	Update(ctx context.Context, e Event) (Event, error)
	Delete(ctx context.Context, id uuid.UUID) error
	AddImages(ctx context.Context, eventID uuid.UUID, ids []uuid.UUID) (Event, error)
	RemoveImages(ctx context.Context, eventID uuid.UUID, ids []uuid.UUID) (Event, error)
	GetDay(ctx context.Context, id uuid.UUID) (Day, error)
	GetDayIncludingArchived(ctx context.Context, id uuid.UUID) (Day, error)
	CreateDay(ctx context.Context, d Day) (Day, error)
	ListDays(ctx context.Context, eventID uuid.UUID) ([]Day, error)
	ListDaysLifecycle(ctx context.Context, eventID uuid.UUID, visibility lifecycle.Visibility) ([]Day, error)
	UpdateDay(ctx context.Context, d Day) (Day, error)
	DeleteDay(ctx context.Context, id uuid.UUID) error
	ListBySeason(ctx context.Context, seasonID uuid.UUID) ([]Event, error)
	SetSeason(ctx context.Context, eventID uuid.UUID, seasonID *uuid.UUID) (Event, error)
	SetMailListID(ctx context.Context, eventID, listID uuid.UUID) (Event, error)
	GetSession(ctx context.Context, id uuid.UUID) (Session, error)
	GetSessionIncludingArchived(ctx context.Context, id uuid.UUID) (Session, error)
	ListSessions(ctx context.Context, eventDayID uuid.UUID) ([]Session, error)
	ListSessionsLifecycle(ctx context.Context, eventDayID uuid.UUID, visibility lifecycle.Visibility) ([]Session, error)
	CreateSession(ctx context.Context, s Session) (Session, error)
	UpdateSession(ctx context.Context, s Session) (Session, error)
	DeleteSession(ctx context.Context, id uuid.UUID) error
}
