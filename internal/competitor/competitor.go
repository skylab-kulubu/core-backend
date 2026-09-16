package competitor

import (
	"time"

	"github.com/google/uuid"
)

type Competitor struct {
	ID        uuid.UUID `json:"id"`
	UserID    uuid.UUID `json:"userId"`
	EventID   uuid.UUID `json:"eventId"`
	Score     *float64  `json:"score,omitempty"`
	IsWinner  bool      `json:"isWinner"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
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
