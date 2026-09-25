package media

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
)

// Role is what a Media is to the record a Media attachment links it to.
type Role string

// The roles of core's own links. The database writes their Media
// attachments in the transaction that writes the link (migration
// 20260926120000).
const (
	RoleEventCover       Role = "event_cover"
	RoleEventGallery     Role = "event_gallery"
	RoleProfilePicture   Role = "profile_picture"
	RoleCertificateAsset Role = "certificate_asset"
)

// DetachedRetention is how long a Media is kept after its last Media
// attachment is removed, the recovery window of an archived Media. The
// database sets it when it detaches a Media.
const DetachedRetention = 30 * 24 * time.Hour

// rolePurposes are the Media purposes each role accepts.
//
// Transition rule: a legacy Media fits every role, as any Media could be
// linked anywhere before Media purpose. superadmin, Skyforms and CMS still
// upload without a purpose; the rule ends when they send one (media redesign
// tickets 09 and 15).
var rolePurposes = map[Role][]string{
	RoleEventCover:       {PurposeEventCover},
	RoleEventGallery:     {PurposeEventGallery},
	RoleProfilePicture:   {PurposeProfilePicture},
	RoleCertificateAsset: {PurposeCertificateAsset},
}

// Link refusals. Each also matches ErrInvalid or ErrForbidden.
var (
	// ErrPurposeMismatch: the Media's purpose does not fit the role, such as
	// a PDF uploaded for a CMS page linked as an Event cover.
	ErrPurposeMismatch = fmt.Errorf("media: its purpose does not fit the role: %w", ErrInvalid)
	// ErrNotLinkable: there is no such Media, or it is archived, its blob is
	// purged or being purged, or it expired unattached.
	ErrNotLinkable = fmt.Errorf("media: cannot be linked: %w", ErrInvalid)
	// ErrTeamMismatch: the Media is on an Event of another Owner team (Team
	// media library).
	ErrTeamMismatch = fmt.Errorf("media: used on another Owner team's Event: %w", ErrForbidden)
)

// LinkRefusal is a link refused for its Media. errors.Is matches its Err.
type LinkRefusal struct {
	Err     error
	MediaID uuid.UUID
	Role    Role
	// Purpose is the Media's purpose, with ErrPurposeMismatch.
	Purpose string
}

func (r *LinkRefusal) Error() string {
	return fmt.Sprintf("%v (media %s as %s)", r.Err, r.MediaID, r.Role)
}

func (r *LinkRefusal) Unwrap() error { return r.Err }

// Linker checks a Media before a core record links it.
type Linker interface {
	// CheckLink refuses, with a *LinkRefusal, a Media that may not be linked
	// in the role: one that cannot be linked at all, or whose purpose does
	// not fit the role.
	CheckLink(ctx context.Context, id uuid.UUID, role Role) error
}

// NewLinker is the Linker over the Media in store.
func NewLinker(store Store) Linker {
	return linker{store: store}
}

type linker struct {
	store Store
}

func (l linker) CheckLink(ctx context.Context, id uuid.UUID, role Role) error {
	m, err := l.store.GetIncludingDeleted(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return &LinkRefusal{Err: ErrNotLinkable, MediaID: id, Role: role}
	}
	if err != nil {
		return err
	}
	if !linkable(m, time.Now()) {
		return &LinkRefusal{Err: ErrNotLinkable, MediaID: id, Role: role}
	}
	if m.Purpose != PurposeLegacy && !slices.Contains(rolePurposes[role], m.Purpose) {
		return &LinkRefusal{Err: ErrPurposeMismatch, MediaID: id, Role: role, Purpose: m.Purpose}
	}
	return nil
}

// linkable reports whether m may get a new Media attachment at now: it is
// not archived, no purge has started, and it has not expired. A detached
// Media inside its 30 days may be linked again.
func linkable(m Media, now time.Time) bool {
	if m.DeletedAt != nil || m.BlobPurgeStartedAt != nil || m.BlobPurgedAt != nil {
		return false
	}
	return m.ExpiresAt == nil || m.ExpiresAt.After(now)
}
