package media

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/lifecycle"
)

var (
	ErrNotFound             = errors.New("media: not found")
	ErrForbidden            = errors.New("media: forbidden")
	ErrInvalid              = errors.New("media: invalid")
	ErrPurged               = errors.New("media: blob purged")
	ErrPurgeInProgress      = errors.New("media: blob purge in progress")
	ErrPublicationUncertain = errors.New("media: publication outcome uncertain")
)

type Media struct {
	ID         uuid.UUID `json:"id"`
	Name       string    `json:"name"`
	Type       string    `json:"type"`
	URL        string    `json:"url"`
	Size       int64     `json:"size"`
	UploadedBy uuid.UUID `json:"uploadedBy"`
	Kind       string    `json:"kind"`
	// Width and Height are an image's size in pixels as shown (upright):
	// after re-encoding for a purpose that re-encodes. Zero when core does
	// not know it: not an image, uploaded without a purpose, or stored
	// before core recorded sizes.
	Width  int `json:"width,omitempty"`
	Height int `json:"height,omitempty"`
	// Sizes are the addresses of a raster image's sizes (SizeCard,
	// SizePage), by size name: every size its purpose names in the
	// catalogue (image.sizes). Built when the Media is answered, from the
	// configured base and address mode.
	Sizes map[string]ImageAddress `json:"sizes,omitempty"`
	// SizeObjects are the sizes stored as objects beside the image, by size
	// name (column size_objects): only those smaller than the image itself.
	// Nil until core has made them (an image stored before sizes waits for
	// the size backfill); empty when the image needs none or could not be
	// read.
	SizeObjects map[string]SizeObject `json:"-"`
	// Purpose is the Media purpose the file was uploaded for; legacy for
	// Media uploaded without a purpose and for Media stored before purposes
	// existed.
	Purpose string `json:"purpose"`
	// Status is pending until a Media attachment links the Media to a
	// record, attached while one does, and detached once the last one is
	// removed. Archive and purge are recorded apart (DeletedAt,
	// BlobPurgedAt).
	Status Status `json:"status"`
	// ExpiresAt is when a Media no Media attachment keeps is purged: a
	// pending Media when its purpose's pending TTL runs out, a detached one
	// 30 days after its last Media attachment was removed. Nil keeps the
	// Media: it is attached, or legacy (a legacy Media never gets an expiry).
	ExpiresAt           *time.Time `json:"expiresAt,omitempty"`
	Key                 string     `json:"-"`
	CoverColors         []string   `json:"coverColors"`
	CoverColorsComputed bool       `json:"-"`
	DeletedAt           *time.Time `json:"deletedAt,omitempty"`
	DeletedBy           *uuid.UUID `json:"deletedBy,omitempty"`
	BlobPurgeStartedAt  *time.Time `json:"blobPurgeStartedAt,omitempty"`
	BlobPurgedAt        *time.Time `json:"blobPurgedAt,omitempty"`
	BlobPurgeCheckedAt  *time.Time `json:"-"`
	CreatedAt           time.Time  `json:"createdAt"`
	UpdatedAt           time.Time  `json:"updatedAt"`

	// ServingPolicyApplied is set once the object's metadata is known to
	// follow the serving policy: set by Upload, or by the serving policy backfill.
	ServingPolicyApplied bool `json:"-"`
}

// imageSize is the image's recorded size; zero when core does not know it.
func (m Media) imageSize() ImageSize {
	return ImageSize{Width: m.Width, Height: m.Height}
}

// Status is where a Media is in its life (Media.Status).
type Status string

const (
	StatusPending  Status = "pending"
	StatusAttached Status = "attached"
	StatusDetached Status = "detached"
)

// expired reports whether the Media's expiry is at or before now. Only a
// Media no Media attachment keeps has one. The database's counterpart is
// expiredSQL.
func (m Media) expired(now time.Time) bool {
	return m.ExpiresAt != nil && !m.ExpiresAt.After(now)
}

// newRecord fills what a Media record takes by default when it is created:
// an id, an empty cover colour list, the legacy purpose when none is given,
// and the pending status. Every Store applies it, and only it.
func newRecord(m Media) Media {
	if m.ID == uuid.Nil {
		m.ID = uuid.New()
	}
	if m.CoverColors == nil {
		m.CoverColors = []string{}
	}
	if m.Purpose == "" {
		m.Purpose = PurposeLegacy
	}
	if m.Status == "" {
		m.Status = StatusPending
	}
	return m
}

type Store interface {
	Create(ctx context.Context, m Media) (Media, error)
	Get(ctx context.Context, id uuid.UUID) (Media, error)
	GetIncludingDeleted(ctx context.Context, id uuid.UUID) (Media, error)
	List(ctx context.Context) ([]Media, error)
	ListLifecycle(ctx context.Context, visibility lifecycle.Visibility) ([]Media, error)
	ListPendingCoverColors(ctx context.Context, limit int) ([]Media, error)
	SetCoverColors(ctx context.Context, id uuid.UUID, colors []string) error
	// ListPendingServingPolicy returns, in id order and after the given id,
	// media whose object may still carry the metadata it was stored with
	// before the serving policy. Media whose blob is purged or being purged
	// is left out.
	ListPendingServingPolicy(ctx context.Context, after uuid.UUID, limit int) ([]Media, error)
	// SetServingPolicyApplied records that the object now follows the serving
	// policy. It changes nothing else on the record.
	SetServingPolicyApplied(ctx context.Context, id uuid.UUID) error
	// ListPendingImageSizes returns, in id order and after the given id,
	// current image Media of the given purposes whose sizes core has not
	// made yet (SizeObjects nil). Media whose blob is purged or being
	// purged are left out.
	ListPendingImageSizes(ctx context.Context, purposes []string, after uuid.UUID, limit int) ([]Media, error)
	// SetImageSizes records an image's size as shown and the sizes stored
	// beside it. Empty objects record that it has none, and nil that they
	// are not made yet (listed again). It refuses with ErrPurgeInProgress or
	// ErrPurged once the blob's purge has begun.
	SetImageSizes(ctx context.Context, id uuid.UUID, size ImageSize, objects map[string]SizeObject) error
	Archive(ctx context.Context, id uuid.UUID, actorID *uuid.UUID) error
	// Restore restores an archived Media and clears its expiry, so an expiry
	// that passed while it was archived cannot purge it.
	Restore(ctx context.Context, id uuid.UUID) error
	// ExpireUnattachedAt sets when a current Media no Media attachment keeps
	// is purged; nil keeps it. An attached Media is left alone.
	ExpireUnattachedAt(ctx context.Context, id uuid.UUID, at *time.Time) error
	ListPurgeCandidates(ctx context.Context, deletedBefore time.Time, limit int) ([]Media, error)
	PurgeBlobIfUnreferenced(ctx context.Context, id uuid.UUID, purgedAt time.Time, purge func(key string) error) (bool, error)
	// ListExpired returns, in id order and after the given id, the Media
	// no Media attachment keeps whose expiry is at or before now: pending
	// Media past their purpose's pending TTL and detached Media past their
	// 30 days. Archived Media are left to the archive window.
	ListExpired(ctx context.Context, now time.Time, after uuid.UUID, limit int) ([]Media, error)
	// PurgeExpiredBlobIfUnattached purges the blob of such a Media with
	// the same checks and two-phase claim as PurgeBlobIfUnreferenced, and
	// archives the Media as its blob goes. It reports false, and keeps the
	// Media, when the Media is no longer expired or a Media attachment or a
	// core link still uses it.
	PurgeExpiredBlobIfUnattached(ctx context.Context, id uuid.UUID, now time.Time, purge func(key string) error) (bool, error)
	// Attach writes a Media attachment another product makes. created is
	// false when the same link already exists; that one is returned. A Media
	// that is gone, archived, or whose purge started is ErrNotLinkable.
	Attach(ctx context.Context, a Attachment) (_ Attachment, created bool, _ error)
	// FindAttachment returns the Media attachment of the same link as a
	// (Media, owner and role); ErrNotFound when there is none.
	FindAttachment(ctx context.Context, a Attachment) (Attachment, error)
	// HeldBy reports whether the product (owner_service) has any Media
	// attachment to the Media.
	HeldBy(ctx context.Context, mediaID uuid.UUID, product authz.Product) (bool, error)
	// GetAttachment returns the Media's attachment with this id; ErrNotFound
	// when the Media has none.
	GetAttachment(ctx context.Context, mediaID, attachmentID uuid.UUID) (Attachment, error)
	// Detach removes the Media's attachment with this id if the service
	// owns it, and does nothing otherwise. A Media whose last Media
	// attachment goes is detached: purged 30 days later, or never for a
	// legacy Media.
	Detach(ctx context.Context, mediaID, attachmentID uuid.UUID, service authz.Product) error
}

// BlobMetadata is how the CDN serves a stored object. An empty
// ContentDisposition leaves the object inline.
type BlobMetadata struct {
	ContentType        string
	ContentDisposition string
}

type BlobStore interface {
	Put(ctx context.Context, key string, data []byte, meta BlobMetadata) error
	// SetMetadata replaces the metadata of a stored object; ErrNotFound when
	// there is no such object.
	SetMetadata(ctx context.Context, key string, meta BlobMetadata) error
	// Read returns an object's bytes; ErrNotFound when there is no such
	// object.
	Read(ctx context.Context, key string) ([]byte, error)
	Delete(ctx context.Context, key string) error
}
