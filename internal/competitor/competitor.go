package competitor

import (
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/event"
)

type Competitor struct {
	ID          uuid.UUID       `json:"id"`
	UserID      uuid.UUID       `json:"userId"`
	EventID     uuid.UUID       `json:"eventId"`
	Event       *event.Resource `json:"event,omitempty"`
	Score       *float64        `json:"score,omitempty"`
	IsWinner    bool            `json:"isWinner"`
	WithdrawnAt *time.Time      `json:"withdrawnAt,omitempty"`
	WithdrawnBy *uuid.UUID      `json:"withdrawnBy,omitempty"`
	CreatedAt   time.Time       `json:"createdAt"`
	UpdatedAt   time.Time       `json:"updatedAt"`
}

type CreateInput struct {
	UserID   uuid.UUID
	EventID  uuid.UUID
	Score    *float64
	IsWinner bool
}

type UpdateInput struct {
	UserID   uuid.UUID
	EventID  uuid.UUID
	Score    *float64
	IsWinner bool
}

type LeaderboardEntry struct {
	UserID     uuid.UUID `json:"userId"`
	TotalScore float64   `json:"totalScore"`
	EventCount int       `json:"eventCount"`
	Rank       int       `json:"rank"`
}
