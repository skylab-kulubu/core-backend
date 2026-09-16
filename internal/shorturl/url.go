package shorturl

import (
	"time"

	"github.com/google/uuid"
)

type URL struct {
	ID         uuid.UUID  `json:"id"`
	Alias      string     `json:"alias"`
	URL        string     `json:"url"`
	ClickCount int        `json:"clickCount"`
	CreatedBy  *uuid.UUID `json:"createdBy,omitempty"`
	CreatedAt  time.Time  `json:"createdAt"`
	UpdatedAt  time.Time  `json:"updatedAt"`
}
