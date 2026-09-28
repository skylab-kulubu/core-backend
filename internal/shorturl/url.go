package shorturl

import (
	"time"

	"github.com/google/uuid"
)

const HitRetention = 90 * 24 * time.Hour

const (
	SourcePersonal = "personal"
	SourceForm     = "form"
	SourceEvent    = "event"
)

type URL struct {
	ID         uuid.UUID  `json:"id"`
	Alias      string     `json:"alias"`
	URL        string     `json:"url"`
	ClickCount int        `json:"clickCount"`
	CreatedBy  *uuid.UUID `json:"createdBy,omitempty"`
	FormID     *uuid.UUID `json:"formId,omitempty"`
	EventID    *uuid.UUID `json:"eventId,omitempty"`
	Label      string     `json:"label,omitempty"`
	DisabledAt *time.Time `json:"disabledAt,omitempty"`
	DisabledBy *uuid.UUID `json:"disabledBy,omitempty"`
	CreatedAt  time.Time  `json:"createdAt"`
	UpdatedAt  time.Time  `json:"updatedAt"`
}

// Source names who manages the link: its event, its form, or the person who
// created it.
func (u URL) Source() string {
	switch {
	case u.EventID != nil:
		return SourceEvent
	case u.FormID != nil:
		return SourceForm
	default:
		return SourcePersonal
	}
}

type Hit struct {
	ID        uuid.UUID  `json:"id"`
	URLID     uuid.UUID  `json:"urlId"`
	Alias     string     `json:"alias"`
	CreatedAt time.Time  `json:"createdAt"`
	IP        string     `json:"ip"`
	UserAgent string     `json:"userAgent"`
	Referer   string     `json:"referer"`
	UTM       UTM        `json:"utm"`
	UserID    *uuid.UUID `json:"userId,omitempty"`
}

// SourceCount is the number of hits under one utm_source; an empty source
// counts the hits that carried no tag. Scans is the part of Count that came
// from a QR code: utm_medium=qr, or the plain utm_source=qr code.
type SourceCount struct {
	Source string `json:"source"`
	Count  int    `json:"count"`
	Scans  int    `json:"scans"`
}

// MediumQR marks a hop that came from scanning a QR code.
const MediumQR = "qr"

func (h Hit) scanned() bool {
	return h.UTM.Medium == MediumQR || h.UTM.Source == MediumQR
}

type Stats struct {
	Since   time.Time     `json:"since"`
	Total   int           `json:"total"`
	Sources []SourceCount `json:"sources"`
}

const (
	ReasonInvalid  = "invalid"
	ReasonReserved = "reserved"
	ReasonTaken    = "taken"
)

type Availability struct {
	Alias     string `json:"alias"`
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
}
