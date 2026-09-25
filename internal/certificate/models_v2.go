package certificate

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

const (
	ScopeClub      = "club"
	ScopeOwnerTeam = "ownerTeam"
	ScopeEvent     = "event"

	SourceEvent       = "event"
	SourceOwnerTeam   = "ownerTeam"
	SourceClubDefault = "clubDefault"
)

type Layout struct {
	Width             float64    `json:"width"`
	Height            float64    `json:"height"`
	Orientation       string     `json:"orientation"`
	BackgroundColor   string     `json:"backgroundColor"`
	BackgroundMediaID *uuid.UUID `json:"backgroundMediaId,omitempty"`
	Elements          []Element  `json:"elements"`
}

type Element struct {
	ID              string     `json:"id"`
	Kind            string     `json:"kind"`
	Text            string     `json:"text,omitempty"`
	MediaID         *uuid.UUID `json:"mediaId,omitempty"`
	X               float64    `json:"x"`
	Y               float64    `json:"y"`
	Width           float64    `json:"width"`
	Height          float64    `json:"height"`
	FontFamily      string     `json:"fontFamily,omitempty"`
	FontSize        float64    `json:"fontSize,omitempty"`
	MinFontSize     float64    `json:"minFontSize,omitempty"`
	FontWeight      int        `json:"fontWeight,omitempty"`
	LineHeight      float64    `json:"lineHeight,omitempty"`
	LetterSpacing   float64    `json:"letterSpacing,omitempty"`
	Color           string     `json:"color,omitempty"`
	BackgroundColor string     `json:"backgroundColor,omitempty"`
	BorderColor     string     `json:"borderColor,omitempty"`
	BorderWidth     float64    `json:"borderWidth,omitempty"`
	BorderRadius    float64    `json:"borderRadius,omitempty"`
	Align           string     `json:"align,omitempty"`
	Rotation        float64    `json:"rotation,omitempty"`
	Opacity         float64    `json:"opacity,omitempty"`
	Fit             string     `json:"fit,omitempty"`
	Locked          bool       `json:"locked,omitempty"`
}

type TemplateDraft struct {
	Name          string `json:"name"`
	OwnerTeam     string `json:"ownerTeam"`
	SourceKind    string `json:"sourceKind"`
	SourceRef     string `json:"sourceRef"`
	SourceEditURL string `json:"sourceEditUrl"`
	Layout        Layout `json:"layout"`
}

type Template struct {
	ID               uuid.UUID        `json:"id"`
	Name             string           `json:"name"`
	OwnerTeam        string           `json:"ownerTeam"`
	SourceKind       string           `json:"sourceKind"`
	SourceRef        string           `json:"sourceRef"`
	SourceEditURL    string           `json:"sourceEditUrl"`
	DraftLayout      Layout           `json:"draftLayout"`
	System           bool             `json:"system"`
	ArchivedAt       *time.Time       `json:"archivedAt,omitempty"`
	CreatedBy        *uuid.UUID       `json:"createdBy,omitempty"`
	CreatedAt        time.Time        `json:"createdAt"`
	UpdatedAt        time.Time        `json:"updatedAt"`
	PublishedVersion *TemplateVersion `json:"publishedVersion,omitempty"`
}

type TemplateVersion struct {
	ID            uuid.UUID                  `json:"id"`
	TemplateID    uuid.UUID                  `json:"templateId"`
	Version       int                        `json:"version"`
	Layout        Layout                     `json:"layout"`
	AssetManifest map[string]VersionAssetRef `json:"-"`
	Checksum      string                     `json:"checksum"`
	PublishedBy   *uuid.UUID                 `json:"publishedBy,omitempty"`
	PublishedAt   time.Time                  `json:"publishedAt"`

	// AssetServingPolicyApplied is set once every asset copy of the version
	// is known to follow the media serving policy: at publish, or by the
	// asset serving backfill.
	AssetServingPolicyApplied bool `json:"-"`
}

type VersionAssetRef struct {
	Key         string `json:"key"`
	ContentType string `json:"contentType"`
}

type Binding struct {
	ID         uuid.UUID  `json:"id"`
	Scope      string     `json:"scope"`
	ScopeKey   string     `json:"scopeKey"`
	TemplateID uuid.UUID  `json:"templateId"`
	UpdatedBy  *uuid.UUID `json:"updatedBy,omitempty"`
	UpdatedAt  time.Time  `json:"updatedAt"`
}

type ResolvedTemplate struct {
	Template Template        `json:"template"`
	Version  TemplateVersion `json:"version"`
	Source   string          `json:"source"`
}

type PreviewData struct {
	RecipientName string `json:"recipientName"`
	EventName     string `json:"eventName"`
	OwnerTeam     string `json:"ownerTeam"`
	Serial        string `json:"serial"`
	EventDates    string `json:"eventDates"`
	IssueDate     string `json:"issueDate"`
}

type Batch struct {
	ID                uuid.UUID  `json:"id"`
	EventID           uuid.UUID  `json:"eventId"`
	TemplateVersionID uuid.UUID  `json:"templateVersionId"`
	TemplateSource    string     `json:"templateSource"`
	Reason            string     `json:"reason"`
	Status            string     `json:"status"`
	RequestedBy       *uuid.UUID `json:"requestedBy,omitempty"`
	TotalCount        int        `json:"totalCount"`
	QueuedCount       int        `json:"queuedCount"`
	IssuedCount       int        `json:"issuedCount"`
	FailedCount       int        `json:"failedCount"`
	CreatedAt         time.Time  `json:"createdAt"`
	StartedAt         *time.Time `json:"startedAt,omitempty"`
	CompletedAt       *time.Time `json:"completedAt,omitempty"`
	Jobs              []Job      `json:"jobs,omitempty"`
}

type Job struct {
	ID            uuid.UUID  `json:"id"`
	BatchID       uuid.UUID  `json:"batchId"`
	EventID       uuid.UUID  `json:"eventId"`
	TicketID      uuid.UUID  `json:"ticketId"`
	Status        string     `json:"status"`
	AttemptCount  int        `json:"attemptCount"`
	NextAttemptAt time.Time  `json:"nextAttemptAt"`
	LeaseUntil    *time.Time `json:"leaseUntil,omitempty"`
	ErrorCode     string     `json:"errorCode,omitempty"`
	CertificateID *uuid.UUID `json:"certificateId,omitempty"`
	CreatedAt     time.Time  `json:"createdAt"`
	StartedAt     *time.Time `json:"startedAt,omitempty"`
	CompletedAt   *time.Time `json:"completedAt,omitempty"`
}

type EventSummary struct {
	EventID               uuid.UUID        `json:"eventId"`
	AttendanceFinalizedAt *time.Time       `json:"attendanceFinalizedAt,omitempty"`
	EligibleCount         int              `json:"eligibleCount"`
	IssuedCount           int              `json:"issuedCount"`
	RevokedCount          int              `json:"revokedCount"`
	QueuedCount           int              `json:"queuedCount"`
	FailedCount           int              `json:"failedCount"`
	Resolution            ResolvedTemplate `json:"resolution"`
}

type PublicCertificate struct {
	Serial        string     `json:"serial"`
	RecipientName string     `json:"recipientName"`
	EventName     string     `json:"eventName"`
	OwnerTeam     string     `json:"ownerTeam"`
	Status        string     `json:"status"`
	VerifyURL     string     `json:"verifyUrl"`
	PDFURL        string     `json:"pdfUrl,omitempty"`
	IssuedAt      time.Time  `json:"issuedAt"`
	RevokedAt     *time.Time `json:"revokedAt,omitempty"`
}

type ShareInfo struct {
	Title string `json:"title"`
	Text  string `json:"text"`
	URL   string `json:"url"`
}

type MineCertificate struct {
	ID        uuid.UUID            `json:"id"`
	Serial    string               `json:"serial"`
	Event     MineCertificateEvent `json:"event"`
	Status    string               `json:"status"`
	IssuedAt  time.Time            `json:"issuedAt"`
	RevokedAt *time.Time           `json:"revokedAt,omitempty"`
	PDFURL    string               `json:"pdfUrl,omitempty"`
	VerifyURL string               `json:"verifyUrl"`
	Share     ShareInfo            `json:"share"`
}

type MineCertificateEvent struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	OwnerTeam string    `json:"ownerTeam"`
}

type TemplateStore interface {
	ListTemplates(ctx context.Context) ([]Template, error)
	GetTemplate(ctx context.Context, id uuid.UUID) (Template, error)
	CreateTemplate(ctx context.Context, t Template) (Template, error)
	UpdateTemplate(ctx context.Context, t Template) (Template, error)
	CreateVersion(ctx context.Context, v TemplateVersion) (TemplateVersion, error)
	LatestVersion(ctx context.Context, templateID uuid.UUID) (TemplateVersion, error)
	GetVersion(ctx context.Context, id uuid.UUID) (TemplateVersion, error)
	GetBinding(ctx context.Context, scope, scopeKey string) (Binding, error)
	ListBindings(ctx context.Context) ([]Binding, error)
	SetBinding(ctx context.Context, b Binding) (Binding, error)
	DeleteBinding(ctx context.Context, scope, scopeKey string) error
}

type JobStore interface {
	CreateBatch(ctx context.Context, b Batch, ticketIDs []uuid.UUID) (Batch, error)
	GetBatch(ctx context.Context, id uuid.UUID) (Batch, error)
	ListBatches(ctx context.Context, eventID uuid.UUID) ([]Batch, error)
	ClaimJobs(ctx context.Context, limit int, lease time.Duration) ([]Job, error)
	CompleteJob(ctx context.Context, jobID, certificateID uuid.UUID) error
	FailJob(ctx context.Context, jobID uuid.UUID, errorCode string, retryAt time.Time, terminal bool) error
	RetryBatch(ctx context.Context, batchID uuid.UUID) error
	FinalizeAttendance(ctx context.Context, eventID, by uuid.UUID) (time.Time, error)
	AttendanceFinalizedAt(ctx context.Context, eventID uuid.UUID) (*time.Time, error)
	JobCounts(ctx context.Context, eventID uuid.UUID) (queued, failed int, err error)
}

type ArtifactStore interface {
	Put(ctx context.Context, key string, data []byte, meta media.BlobMetadata) error
	Read(ctx context.Context, key string) ([]byte, error)
}

type Asset struct {
	ContentType string
	Data        []byte
}

type AssetReader interface {
	ReadAsset(ctx context.Context, id uuid.UUID) (Asset, error)
}

func marshalLayout(layout Layout) ([]byte, error) {
	return json.Marshal(layout)
}
