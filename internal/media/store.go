package media

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
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
	// Purpose is the Media purpose the file was uploaded for; legacy for
	// Media uploaded without a purpose and for Media stored before purposes
	// existed.
	Purpose             string     `json:"purpose"`
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

// newRecord fills what a Media record takes by default when it is created:
// an id, an empty cover colour list, and the legacy purpose when none is
// given. Every Store applies it, and only it.
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
	Archive(ctx context.Context, id uuid.UUID, actorID *uuid.UUID) error
	Restore(ctx context.Context, id uuid.UUID) error
	ListPurgeCandidates(ctx context.Context, deletedBefore time.Time, limit int) ([]Media, error)
	PurgeBlobIfUnreferenced(ctx context.Context, id uuid.UUID, purgedAt time.Time, purge func(key string) error) (bool, error)
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
	Read(ctx context.Context, key string) ([]byte, error)
	Delete(ctx context.Context, key string) error
}
