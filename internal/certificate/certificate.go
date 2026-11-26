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
	// ErrLeaseLost: the job's lease ran out and another claim took it; its
	// holder neither finishes nor puts back the job (ticket 25).
	ErrLeaseLost = errors.New("certificate: job lease lost")
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
	// CoOrganizerTeams are the Event's Co-organizer teams shown outside the
	// panel when it was issued (docs/co-organizers.md): its snapshot, as
	// OwnerTeam, never changed after. Nil for a certificate issued before
	// core kept it, empty for none. Only the public verification names them.
	CoOrganizerTeams []string `json:"-"`
	// Attendance is the attendance it was issued on; nil for a certificate
	// issued before core kept it. Only the public verification shows it.
	Attendance *AttendanceSnapshot `json:"-"`
	// Mail is its latest mail in the Event's list (ListByEvent), nil
	// elsewhere and for a certificate issued before the queue.
	Mail *MailState `json:"mail,omitempty"`
}

// AttendanceSnapshot is a Ticket's attendance at its certificate's issue
// (docs/certificate-rule.md): its check-ins on the live counted Sessions,
// their number N, and the program minutes (end − start) of the counted
// Sessions it attended. It is written with the certificate and never
// changed; a reissue takes a new one.
type AttendanceSnapshot struct {
	Attended       int
	CountedTotal   int
	ProgramMinutes int
}

type Renderer interface {
	PDF(ctx context.Context, html string) ([]byte, error)
}

// PDFBackgroundRenderer preserves a one-page PDF as the vector background and
// renders the editable SKY LAB layout as an overlay on top of it.
type PDFBackgroundRenderer interface {
	PDFWithBackground(ctx context.Context, html string, background []byte) ([]byte, error)
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
	// AutoFinalize is one pass of the automatic attendance finalization at
	// now (MaintainAutoFinalize); Rescan queues who became eligible after
	// the attendance closed (docs/certificate-rule.md).
	AutoFinalize(ctx context.Context, now time.Time) (AutoFinalizeResult, error)
	Rescan(ctx context.Context, p authz.Principal, eventID uuid.UUID) (RescanResult, error)
	Attendance(ctx context.Context, p authz.Principal, eventID uuid.UUID) (AttendanceList, error)
	QueueManual(ctx context.Context, p authz.Principal, eventID, ticketID uuid.UUID, justification string) (Batch, error)
	GetBatch(ctx context.Context, p authz.Principal, id uuid.UUID) (Batch, error)
	ListBatches(ctx context.Context, p authz.Principal, eventID uuid.UUID) ([]Batch, error)
	RetryBatch(ctx context.Context, p authz.Principal, id uuid.UUID) (Batch, error)
	ProcessNext(ctx context.Context, limit int) (int, error)
	// ProcessMails sends due certificate mails (MaintainMails); ResendMail
	// and ResendEventMails queue them again (docs/certificate-mail.md).
	ProcessMails(ctx context.Context, limit int) (int, error)
	ResendMail(ctx context.Context, p authz.Principal, serial string) (MailResend, error)
	ResendEventMails(ctx context.Context, p authz.Principal, eventID uuid.UUID, scope string) (MailResend, error)
	ListTemplates(ctx context.Context, p authz.Principal, filter TemplateFilter) ([]Template, error)
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
	// EventTemplate and CreateEventTemplate are an Event's own template
	// (docs/certificate-event-templates.md).
	EventTemplate(ctx context.Context, p authz.Principal, eventID uuid.UUID) (EventTemplateState, error)
	CreateEventTemplate(ctx context.Context, p authz.Principal, eventID uuid.UUID, in EventTemplateInput) (Template, error)
	// AssetInfo and AssetPreview are a certificate asset's size and the
	// picture the template editor draws it with (pdf-tools for a PDF).
	AssetInfo(ctx context.Context, p authz.Principal, id uuid.UUID) (AssetInfo, error)
	AssetPreview(ctx context.Context, p authz.Principal, id uuid.UUID, maxPx int) (AssetPreview, error)
	// AssetText lists a PDF asset's text and ConvertAssetText turns the
	// chosen pieces into layout elements over a new PDF without them
	// (docs/certificate-template-errors.md, "Background text conversion").
	// templateID (optional) matches the text against the fonts of that
	// template's team and the club's.
	AssetText(ctx context.Context, p authz.Principal, id uuid.UUID, templateID *uuid.UUID) (AssetText, error)
	ConvertAssetText(ctx context.Context, p authz.Principal, id uuid.UUID, req TextConversionRequest) (TextConversion, error)
	// ListFonts, CreateFont, FontFile and ArchiveFont are the teams' and
	// the club's certificate fonts (docs/certificate-fonts.md).
	// ListFonts with templateID is that template's editor's list (team,
	// club, and the fonts its draft keeps); else team's and the club's.
	ListFonts(ctx context.Context, p authz.Principal, team string, templateID *uuid.UUID) ([]Font, error)
	CreateFont(ctx context.Context, p authz.Principal, in FontInput) (Font, error)
	FontFile(ctx context.Context, p authz.Principal, id uuid.UUID) (FontFile, error)
	ArchiveFont(ctx context.Context, p authz.Principal, id uuid.UUID) error
}
