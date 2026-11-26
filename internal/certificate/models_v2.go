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
	// BackgroundPlacement places the background; nil (a layout written
	// before it) prints the background as before (LegacyImagePlacement,
	// LegacyPDFPlacement), and is stored as before, so the checksum of
	// such a layout does not change.
	BackgroundPlacement *BackgroundPlacement `json:"backgroundPlacement,omitempty"`
	// BackgroundOriginalMediaID is the PDF a background's text was
	// converted from (POST /v1/certificate-assets/{id}/text-conversion,
	// docs/certificate-template-errors.md, "Background text conversion"):
	// BackgroundMediaID is then that PDF without the converted text. It is
	// never printed and a published version copies no asset of it; the
	// template keeps it linked so the editor can go back to it.
	BackgroundOriginalMediaID *uuid.UUID `json:"backgroundOriginalMediaId,omitempty"`
	Elements                  []Element  `json:"elements"`
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
	Opacity         *float64   `json:"opacity,omitempty"` // nil (left out) = 1, as the panel's editor shows it; an explicit 0 stays 0
	Fit             string     `json:"fit,omitempty"`
	Locked          bool       `json:"locked,omitempty"`
	// FontStyle is "italic" or empty (normal).
	FontStyle string `json:"fontStyle,omitempty"`
	// Origin is OriginBackgroundText for an element converted from the
	// background PDF's text, empty otherwise. SourceFont is then the PDF's
	// font name, which FontFamily stands in for.
	Origin     string `json:"origin,omitempty"`
	SourceFont string `json:"sourceFont,omitempty"`
	// FontMediaID names the checked file of a team's or the club's
	// certificate font (docs/certificate-fonts.md) a text element is set
	// in; FontFamily stays its name as the editor shows it. The page embeds
	// the file and falls back to FontFamily's stack.
	FontMediaID *uuid.UUID `json:"fontMediaId,omitempty"`
}

// OriginBackgroundText is an element converted from its background PDF's
// text (Element.Origin).
const OriginBackgroundText = "backgroundText"

// The bounds of an element's converted-text fields.
const (
	FontStyleItalic    = "italic"
	maxSourceFontRunes = 120
)

type TemplateDraft struct {
	Name          string `json:"name"`
	OwnerTeam     string `json:"ownerTeam"`
	SourceKind    string `json:"sourceKind"`
	SourceRef     string `json:"sourceRef"`
	SourceEditURL string `json:"sourceEditUrl"`
	Layout        Layout `json:"layout"`
}

type Template struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	OwnerTeam string    `json:"ownerTeam"`
	// EventID names the Event an Event template belongs to: it is bound to
	// that Event only and its rules follow the Event's
	// (docs/certificate-event-templates.md). Nil for a team or club
	// template.
	EventID          *uuid.UUID       `json:"eventId,omitempty"`
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
	// Encryption opens the copy of a private asset, which is kept encrypted
	// in the private bucket; nil for a public copy.
	Encryption *media.Encryption `json:"encryption,omitempty"`
	// Placed is a PDF background's copy placed on the version's page by
	// its placement (pdf-tools /place), which every render of the version
	// prints instead of placing it again; nil for an image, a layout
	// without a placement, and a version published before placed copies.
	Placed *PlacedAssetRef `json:"placed,omitempty"`
}

// PlacedAssetRef is a version's placed copy of its PDF background, stored
// beside the asset's copy and, for a private asset, as encrypted as it.
type PlacedAssetRef struct {
	Key        string            `json:"key"`
	Encryption *media.Encryption `json:"encryption,omitempty"`
	// Checksum is the placed PDF's SHA-256, hex; a copy that reads back
	// otherwise is not printed.
	Checksum string `json:"checksum"`
}

// Sealed is the placed copy's private object; false for a public copy.
func (r PlacedAssetRef) Sealed() (media.SealedObject, bool) {
	if r.Encryption == nil {
		return media.SealedObject{}, false
	}
	return media.SealedObject{Key: r.Key, Encryption: *r.Encryption}, true
}

// Sealed is the copy's private object; false for a public copy.
func (r VersionAssetRef) Sealed() (media.SealedObject, bool) {
	if r.Encryption == nil {
		return media.SealedObject{}, false
	}
	return media.SealedObject{Key: r.Key, Encryption: *r.Encryption}, true
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
	// Justification is why a manual issue was given, required below the
	// rule's threshold (docs/certificate-rule.md); with RequestedBy and
	// CreatedAt it is the manual issue's audit record.
	Justification *string `json:"justification,omitempty"`
	// FinalizedSource is, on a finalization batch, who closes the
	// attendance with it: FinalizedManual (the default) or FinalizedAuto.
	FinalizedSource string     `json:"-"`
	TotalCount      int        `json:"totalCount"`
	QueuedCount     int        `json:"queuedCount"`
	IssuedCount     int        `json:"issuedCount"`
	FailedCount     int        `json:"failedCount"`
	CreatedAt       time.Time  `json:"createdAt"`
	StartedAt       *time.Time `json:"startedAt,omitempty"`
	CompletedAt     *time.Time `json:"completedAt,omitempty"`
	Jobs            []Job      `json:"jobs,omitempty"`
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
	// LeaseToken names the claim that holds the job; only that claim may
	// renew, complete or fail it.
	LeaseToken uuid.UUID `json:"-"`
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
	// CountedTotal is N, the live counted Sessions; EffectiveMin the fewest
	// counted check-ins the rule takes over them; BelowCount the Tickets
	// with no certificate, no job and not eligible (the list's "below").
	CountedTotal int `json:"countedTotal"`
	EffectiveMin int `json:"effectiveMin"`
	BelowCount   int `json:"belowCount"`
	// Sessionless: the Event has no live Session, so N is the Event itself
	// (1) and a Venue entry is its attendance (docs/certificate-rule.md).
	Sessionless bool `json:"sessionless"`
	// AutoFinalizeAt is when the attendance closes by itself: an hour after
	// the last live Session ends, while the rule issues, the Event's switch
	// is on, it is published and not closed yet, and that time is within
	// AutoFinalizeWindow (docs/certificate-rule.md). FinalizedSource is who
	// closed it, once closed: "manual" or "auto".
	AutoFinalizeAt  *time.Time `json:"autoFinalizeAt,omitempty"`
	FinalizedSource string     `json:"finalizedSource,omitempty"`
	// MailQueuedCount and MailFailedCount are the active certificates whose
	// latest mail waits or failed (docs/certificate-mail.md).
	MailQueuedCount int `json:"mailQueuedCount"`
	MailFailedCount int `json:"mailFailedCount"`
}

type PublicCertificate struct {
	Serial        string `json:"serial"`
	RecipientName string `json:"recipientName"`
	EventName     string `json:"eventName"`
	OwnerTeam     string `json:"ownerTeam"`
	OrganizerName string `json:"organizerName"` // organizer.Name(OwnerTeam)
	// CoOrganizerNames names the Co-organizer teams shown outside the panel
	// when it was issued (its snapshot, docs/co-organizers.md); left out
	// for none and for a certificate issued before core kept it.
	CoOrganizerNames string     `json:"coOrganizerNames,omitempty"`
	Status           string     `json:"status"`
	VerifyURL        string     `json:"verifyUrl"`
	PDFURL           string     `json:"pdfUrl,omitempty"`
	IssuedAt         time.Time  `json:"issuedAt"`
	RevokedAt        *time.Time `json:"revokedAt,omitempty"`
	// Attendance is the attendance the certificate was issued on, absent
	// for one issued before core kept it. It names no Session, check-in
	// time or manual issue.
	Attendance *PublicAttendance `json:"attendance,omitempty"`
}

// PublicAttendance is a certificate's attendance as the verification page
// shows it: "8 oturumdan 6'sı · 9 saat".
type PublicAttendance struct {
	Attended       int `json:"attended"`
	Total          int `json:"total"`
	ProgramMinutes int `json:"programMinutes"`
}

// publicAttendance is the snapshot as the public verification answers it.
func publicAttendance(a *AttendanceSnapshot) *PublicAttendance {
	if a == nil {
		return nil
	}
	return &PublicAttendance{Attended: a.Attended, Total: a.CountedTotal, ProgramMinutes: a.ProgramMinutes}
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
	// OrganizerName is organizer.Name(OwnerTeam), as PublicCertificate's.
	OrganizerName string `json:"organizerName"`
}

// TemplateFilter narrows ListTemplates. The zero filter lists the team and
// club templates, never an Event template; EventID lists only that Event's.
type TemplateFilter struct {
	EventID *uuid.UUID
}

type TemplateStore interface {
	// ListTemplates lists the current (not archived) templates filter
	// names. CreateTemplate answers ErrConflict for a second current Event
	// template of an Event.
	ListTemplates(ctx context.Context, filter TemplateFilter) ([]Template, error)
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
	// RenewJobLease, CompleteJob and FailJob return ErrLeaseLost unless the
	// job is still running under leaseToken.
	RenewJobLease(ctx context.Context, jobID, leaseToken uuid.UUID, lease time.Duration) error
	CompleteJob(ctx context.Context, jobID, leaseToken, certificateID uuid.UUID) error
	FailJob(ctx context.Context, jobID, leaseToken uuid.UUID, errorCode string, retryAt time.Time, terminal bool) error
	RetryBatch(ctx context.Context, batchID uuid.UUID) error
	FinalizeAttendance(ctx context.Context, eventID, by uuid.UUID) (time.Time, error)
	AttendanceFinalizedAt(ctx context.Context, eventID uuid.UUID) (*time.Time, error)
	// AttendanceFinalization is the Event's closed attendance: when, by
	// whom and how; nil while it is open.
	AttendanceFinalization(ctx context.Context, eventID uuid.UUID) (*Finalization, error)
	JobCounts(ctx context.Context, eventID uuid.UUID) (queued, failed int, err error)
}

type ArtifactStore interface {
	Put(ctx context.Context, key string, data []byte, meta media.BlobMetadata) error
	Read(ctx context.Context, key string) ([]byte, error)
}

type Asset struct {
	ContentType string
	Data        []byte
	// Sealed is the private object of an asset read from a private Media
	// (or from a version's private copy): the service decrypts it into Data,
	// and a published version keeps its copy encrypted, never in the public
	// bucket.
	Sealed *media.SealedObject
}

type AssetReader interface {
	ReadAsset(ctx context.Context, id uuid.UUID) (Asset, error)
}

func marshalLayout(layout Layout) ([]byte, error) {
	return json.Marshal(layout)
}
