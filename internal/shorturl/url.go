package shorturl

import (
	"time"

	"github.com/google/uuid"
)

const HitRetention = 90 * 24 * time.Hour

type URL struct {
	ID         uuid.UUID  `json:"id"`
	Alias      string     `json:"alias"`
	URL        string     `json:"url"`
	ClickCount int        `json:"clickCount"`
	CreatedBy  *uuid.UUID `json:"createdBy,omitempty"`
	CreatedAt  time.Time  `json:"createdAt"`
	UpdatedAt  time.Time  `json:"updatedAt"`
}

type Hit struct {
	ID        uuid.UUID  `json:"id"`
	URLID     uuid.UUID  `json:"urlId"`
	Alias     string     `json:"alias"`
	CreatedAt time.Time  `json:"createdAt"`
	IP        string     `json:"ip"`
	UserAgent string     `json:"userAgent"`
	Referer   string     `json:"referer"`
	UserID    *uuid.UUID `json:"userId,omitempty"`
}
