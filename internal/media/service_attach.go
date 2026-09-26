package media

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
)

// Owner is the record a Media attachment links a Media to: the product that
// owns it, its type there and its id.
type Owner struct {
	Service authz.Product `json:"service"`
	Type    string        `json:"type"`
	ID      uuid.UUID     `json:"id"`
}

// Attachment is one Media attachment: a Media in a role on an owner's record.
type Attachment struct {
	ID        uuid.UUID `json:"id"`
	MediaID   uuid.UUID `json:"mediaId"`
	Owner     Owner     `json:"owner"`
	Role      Role      `json:"role"`
	CreatedAt time.Time `json:"createdAt"`
}

// ErrAttachForbidden refuses a caller of the service attach API that is not
// a product's service account with the media:attach role on the core
// client: a person, whatever roles they hold, a service account without the
// role, or one of a client that is no product core knows.
var ErrAttachForbidden = fmt.Errorf("media: only a product's service account may manage Media attachments: %w", ErrForbidden)

// ErrAttachWrongService refuses a Media attachment of a record another
// product (or core) owns: a product manages only its own.
var ErrAttachWrongService = fmt.Errorf("media: the Media attachment belongs to another product: %w", ErrForbidden)

// ErrProductMismatch refuses a link to a private Media by a product that
// does not own it: only the owning product of its purpose (Skyforms for an
// Answer file, core for a certificate asset) may link it.
var ErrProductMismatch = fmt.Errorf("media: the Media is private to another product: %w", ErrForbidden)

// ErrRoleUnknown refuses a role the calling product's records do not give a
// Media (serviceRoles).
var ErrRoleUnknown = fmt.Errorf("media: not a role of the product's records: %w", ErrInvalid)

// ownerType is how a product names the type of its records: lowercase
// snake_case, at most 64 characters.
var ownerType = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// Attach links the Media to a record of the calling product. created is false
// when the same link already exists; that Media attachment is returned.
func (s *service) Attach(ctx context.Context, p authz.Principal, mediaID uuid.UUID, owner Owner, role Role) (Attachment, bool, error) {
	product, err := s.attachingProduct(p, authz.Create)
	if err != nil {
		return Attachment{}, false, err
	}
	if owner.Service != product {
		return Attachment{}, false, ErrAttachWrongService
	}
	if !ownerType.MatchString(owner.Type) || owner.ID == uuid.Nil {
		return Attachment{}, false, ErrInvalid
	}
	if _, known := rolePurposes[product][role]; !known {
		return Attachment{}, false, ErrRoleUnknown
	}
	link := Attachment{MediaID: mediaID, Owner: owner, Role: role}
	existing, err := s.media.FindAttachment(ctx, link)
	if err == nil {
		// A retry: the link exists, whatever became of the Media since.
		return existing, false, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Attachment{}, false, err
	}
	refused := func(reason error) (Attachment, bool, error) {
		return Attachment{}, false, &LinkRefusal{Err: reason, MediaID: mediaID, Role: role}
	}
	m, err := s.media.GetIncludingDeleted(ctx, mediaID)
	if errors.Is(err, ErrNotFound) {
		return refused(ErrNotLinkable)
	}
	if err != nil {
		return Attachment{}, false, err
	}
	if !linkable(m, time.Now()) {
		return refused(ErrNotLinkable)
	}
	if purpose, _ := s.catalogue.Lookup(m.Purpose); purpose.Visibility == VisibilityPrivate && purpose.OwningProduct() != product {
		// Before the role check, so that the refusal does not tell another
		// product what the Media is.
		return refused(ErrProductMismatch)
	}
	if !fits(product, role, m.Purpose) {
		return Attachment{}, false, &LinkRefusal{Err: ErrPurposeMismatch, MediaID: mediaID, Role: role, Purpose: m.Purpose}
	}
	created, isNew, err := s.media.Attach(ctx, link)
	if errors.Is(err, ErrNotLinkable) {
		// Archived or claimed by a purge since it was read.
		return refused(ErrNotLinkable)
	}
	return created, isNew, err
}

// attachingProduct is the product whose service account p is, when p may
// manage Media attachments.
func (s *service) attachingProduct(p authz.Principal, action authz.Action) (authz.Product, error) {
	if !s.authz.Allow(p, authz.Resource{Type: authz.TypeMediaAttachment}, action) {
		return "", ErrAttachForbidden
	}
	return p.Product, nil
}

// Detach removes a Media attachment of the calling product. Removing one
// that is not there (any more) succeeds, so a retry does too. When it was the
// Media's last, the Media is detached and purged 30 days later unless
// something attaches it again (a legacy Media gets no expiry).
func (s *service) Detach(ctx context.Context, p authz.Principal, mediaID, attachmentID uuid.UUID) error {
	product, err := s.attachingProduct(p, authz.Delete)
	if err != nil {
		return err
	}
	a, err := s.media.GetAttachment(ctx, mediaID, attachmentID)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if a.Owner.Service != product {
		return ErrAttachWrongService
	}
	return s.media.Detach(ctx, mediaID, attachmentID, product)
}
