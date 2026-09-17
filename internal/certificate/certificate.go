package certificate

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
)

var (
	ErrNotFound  = errors.New("certificate: not found")
	ErrForbidden = errors.New("certificate: forbidden")
	ErrInvalid   = errors.New("certificate: invalid")
	ErrConflict  = errors.New("certificate: conflict")
)

type Certificate struct {
	ID             uuid.UUID  `json:"id"`
	EventID        uuid.UUID  `json:"eventId"`
	TicketID       uuid.UUID  `json:"ticketId"`
	OwnerID        *uuid.UUID `json:"ownerId,omitempty"`
	Serial         string     `json:"serial"`
	RecipientName  string     `json:"recipientName"`
	RecipientEmail string     `json:"recipientEmail"`
	EventName      string     `json:"eventName"`
	OwnerTeam      string     `json:"ownerTeam"`
	VerifyURL      string     `json:"verifyUrl"`
	RevokedAt      *time.Time `json:"revokedAt,omitempty"`
	IssuedAt       time.Time  `json:"issuedAt"`
}

type Renderer interface {
	PDF(ctx context.Context, html string) ([]byte, error)
}

type Mailer interface {
	Certificate(ctx context.Context, recipientEmail, fullName string, vars map[string]string)
}

type Store interface {
	Create(ctx context.Context, c Certificate, pdf []byte) (Certificate, error)
	GetBySerial(ctx context.Context, serial string) (Certificate, []byte, error)
	GetActive(ctx context.Context, eventID, ticketID uuid.UUID) (Certificate, error)
	ListByOwner(ctx context.Context, ownerID uuid.UUID) ([]Certificate, error)
	ListByEvent(ctx context.Context, eventID uuid.UUID) ([]Certificate, error)
	Revoke(ctx context.Context, serial string) (Certificate, error)
}

type Service interface {
	RecomputeTicket(ctx context.Context, ticketID uuid.UUID) (*Certificate, error)
	RecomputeEvent(ctx context.Context, p authz.Principal, eventID uuid.UUID) ([]Certificate, error)
	Issue(ctx context.Context, p authz.Principal, eventID, ticketID uuid.UUID) (Certificate, error)
	Revoke(ctx context.Context, p authz.Principal, serial string) error
	Verify(ctx context.Context, serial string) (Certificate, error)
	PDF(ctx context.Context, serial string) ([]byte, error)
	Mine(ctx context.Context, p authz.Principal) ([]Certificate, error)
	ListByEvent(ctx context.Context, p authz.Principal, eventID uuid.UUID) ([]Certificate, error)
}
