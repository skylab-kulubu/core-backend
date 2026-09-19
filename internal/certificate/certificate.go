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
	ID                uuid.UUID  `json:"id"`
	EventID           uuid.UUID  `json:"eventId"`
	TicketID          uuid.UUID  `json:"ticketId"`
	OwnerID           *uuid.UUID `json:"ownerId,omitempty"`
	Serial            string     `json:"serial"`
	RecipientName     string     `json:"recipientName"`
	RecipientEmail    string     `json:"recipientEmail"`
	EventName         string     `json:"eventName"`
	OwnerTeam         string     `json:"ownerTeam"`
	VerifyURL         string     `json:"verifyUrl"`
	TemplateVersionID *uuid.UUID `json:"templateVersionId,omitempty"`
	TemplateSource    string     `json:"templateSource"`
	PDFKey            string     `json:"-"`
	PDFSHA256         string     `json:"pdfSha256,omitempty"`
	BatchID           *uuid.UUID `json:"batchId,omitempty"`
	JobID             *uuid.UUID `json:"jobId,omitempty"`
	RevokedAt         *time.Time `json:"revokedAt,omitempty"`
	IssuedAt          time.Time  `json:"issuedAt"`
}

type Renderer interface {
	PDF(ctx context.Context, html string) ([]byte, error)
}

type Mailer interface {
	Certificate(ctx context.Context, recipientEmail, fullName string, vars map[string]string)
}

type Store interface {
	Create(ctx context.Context, c Certificate, pdf []byte) (Certificate, error)
	Replace(ctx context.Context, previousSerial string, c Certificate, pdf []byte) (Certificate, error)
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
	Reissue(ctx context.Context, p authz.Principal, serial string) (Batch, error)
	Verify(ctx context.Context, serial string) (PublicCertificate, error)
	PDF(ctx context.Context, serial string) ([]byte, error)
	Mine(ctx context.Context, p authz.Principal) ([]MineCertificate, error)
	ListByEvent(ctx context.Context, p authz.Principal, eventID uuid.UUID) ([]Certificate, error)
	Summary(ctx context.Context, p authz.Principal, eventID uuid.UUID) (EventSummary, error)
	Finalize(ctx context.Context, p authz.Principal, eventID uuid.UUID) (Batch, error)
	QueueManual(ctx context.Context, p authz.Principal, eventID, ticketID uuid.UUID) (Batch, error)
	GetBatch(ctx context.Context, p authz.Principal, id uuid.UUID) (Batch, error)
	ListBatches(ctx context.Context, p authz.Principal, eventID uuid.UUID) ([]Batch, error)
	RetryBatch(ctx context.Context, p authz.Principal, id uuid.UUID) (Batch, error)
	ProcessNext(ctx context.Context, limit int) (int, error)
	ListTemplates(ctx context.Context, p authz.Principal) ([]Template, error)
	GetTemplate(ctx context.Context, p authz.Principal, id uuid.UUID) (Template, error)
	CreateTemplate(ctx context.Context, p authz.Principal, in TemplateDraft) (Template, error)
	UpdateTemplate(ctx context.Context, p authz.Principal, id uuid.UUID, in TemplateDraft) (Template, error)
	PublishTemplate(ctx context.Context, p authz.Principal, id uuid.UUID) (TemplateVersion, error)
	PreviewTemplate(ctx context.Context, p authz.Principal, id uuid.UUID, sample PreviewData) ([]byte, error)
	PreviewEvent(ctx context.Context, p authz.Principal, eventID uuid.UUID) ([]byte, error)
	ResolveTemplate(ctx context.Context, p authz.Principal, eventID uuid.UUID) (ResolvedTemplate, error)
	ListBindings(ctx context.Context, p authz.Principal) ([]Binding, error)
	SetBinding(ctx context.Context, p authz.Principal, in Binding) (Binding, error)
	ClearBinding(ctx context.Context, p authz.Principal, scope, scopeKey string) error
}
