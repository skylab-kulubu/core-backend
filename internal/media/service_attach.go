package media

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
)

// Owner is the record a Media attachment links a Media to: the product that
// owns it, its type there and its id. The id is the product's own: a Skyforms
// response id, a CMS block or collection item id, or a CMS page as
// clientId:slug.
type Owner struct {
	Service authz.Product `json:"service"`
	Type    string        `json:"type"`
	ID      string        `json:"id"`
}

// Attachment is one Media attachment: a Media in a role on an owner's record.
type Attachment struct {
	ID        uuid.UUID `json:"id"`
	MediaID   uuid.UUID `json:"mediaId"`
	Owner     Owner     `json:"owner"`
	Role      Role      `json:"role"`
	CreatedAt time.Time `json:"createdAt"`
}

// AttachRequest is a product's request to link a Media to one of its
// records.
type AttachRequest struct {
	Owner Owner
	Role  Role
	// OnBehalfOf is the person the product acts for: the respondent whose
	// Skyforms answer it is, the editor saving the CMS page.
	OnBehalfOf uuid.UUID
}

// personalPurposes are the purposes whose Media belong to the person who
// uploaded them: a product links one only for that person.
var personalPurposes = []string{PurposeAnswerFile, PurposeAnswerFileLarge}

// ErrAttachForbidden refuses a caller of the service attach API that is not
// a product's service account with the media:attach role on the core
// client: a person, whatever roles they hold, a service account without the
// role, or one of a client that is no product's configured service client.
var ErrAttachForbidden = fmt.Errorf("media: only a product's service account may manage Media attachments: %w", ErrForbidden)

// ErrAttachWrongService refuses a Media attachment of a record another
// product (or core) owns: a product manages only its own.
var ErrAttachWrongService = fmt.Errorf("media: the Media attachment belongs to another product: %w", ErrForbidden)

// ErrRoleUnknown refuses a role the calling product's records do not give a
// Media (rolePurposes).
var ErrRoleUnknown = fmt.Errorf("media: not a role of the product's records: %w", ErrInvalid)

// RoleRefusal is an attach request naming a role that is not the calling
// product's. errors.Is matches ErrRoleUnknown.
type RoleRefusal struct {
	Role Role
}

func (r *RoleRefusal) Error() string { return fmt.Sprintf("%v (%q)", ErrRoleUnknown, r.Role) }

func (r *RoleRefusal) Unwrap() error { return ErrRoleUnknown }

var (
	// ownerType is how a product names the type of its records: lowercase
	// snake_case, at most 64 characters.
	ownerType = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	// ownerID is a record's id in its product: at most 200 letters, digits
	// and - _ . : / (a UUID, or a CMS page as clientId:slug).
	ownerID = regexp.MustCompile(`^[A-Za-z0-9._:/-]{1,200}$`)
)

// Attach links the Media to a record of the calling product, for the person
// the product acts for. created is false when the same link already exists;
// that Media attachment is returned.
//
// The caller is authorized before the request is read. A Media the product
// may not link (another product's or core's, a personal one of another
// person, a legacy one it has no claim on) is refused like a Media that does
// not exist.
func (s *service) Attach(ctx context.Context, p authz.Principal, mediaID uuid.UUID, req AttachRequest) (Attachment, bool, error) {
	product, err := s.attachingProduct(p, authz.Create)
	if err != nil {
		return Attachment{}, false, err
	}
	if mediaID == uuid.Nil || req.OnBehalfOf == uuid.Nil || req.Owner.Service == "" ||
		!ownerType.MatchString(req.Owner.Type) || !ownerID.MatchString(req.Owner.ID) {
		return Attachment{}, false, ErrInvalid
	}
	if req.Owner.Service != product {
		return Attachment{}, false, ErrAttachWrongService
	}
	if _, known := rolePurposes[product][req.Role]; !known {
		return Attachment{}, false, &RoleRefusal{Role: req.Role}
	}
	link := Attachment{MediaID: mediaID, Owner: req.Owner, Role: req.Role}
	existing, err := s.media.FindAttachment(ctx, link)
	if err == nil {
		// A retry: the link exists, whatever became of the Media since.
		return existing, false, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Attachment{}, false, err
	}
	may := func(m Media) (bool, error) { return s.mayLink(ctx, product, m, req.OnBehalfOf) }
	m, err := checkLink(ctx, s.media, mediaID, product, req.Role, may)
	if errors.Is(err, ErrPurposeMismatch) && m.DetachExpiryHeld {
		return s.attachHeld(ctx, product, link)
	}
	if err != nil {
		return Attachment{}, false, err
	}
	created, isNew, err := s.media.Attach(ctx, link)
	if errors.Is(err, ErrNotLinkable) {
		// Archived or claimed by a purge since it was read.
		return Attachment{}, false, &LinkRefusal{Err: ErrNotLinkable, MediaID: mediaID, Role: req.Role}
	}
	return created, isNew, err
}

// attachHeld links a Media whose detach expiry the legacy backfill holds,
// and whose purpose, given from core's uses, does not fit the product's
// role: the use the hold protected until stage 5. The Media goes back to
// legacy, as a Media with mixed uses does (K1), and is attached.
func (s *service) attachHeld(ctx context.Context, product authz.Product, link Attachment) (Attachment, bool, error) {
	created, isNew, demotedFrom, err := s.media.AttachHeld(ctx, link)
	if errors.Is(err, ErrNotLinkable) {
		// Released, archived or claimed by a purge since it was read.
		return Attachment{}, false, &LinkRefusal{Err: ErrNotLinkable, MediaID: link.MediaID, Role: link.Role}
	}
	if err == nil && demotedFrom != "" {
		log.Printf("media %s: held %s Media linked by %s as %s; back to legacy", link.MediaID, demotedFrom, product, link.Role)
	}
	return created, isNew, err
}

// mayLink reports whether the product may link m at all, for the person it
// acts for:
//   - a Media of one of the product's own purposes; a personal one only for
//     the person who uploaded it;
//   - a legacy Media for the person who uploaded it, or one the product
//     already holds a Media attachment to (an editor reusing a library
//     image), so that no product pins another product's or core's legacy
//     Media. A Media whose detach expiry the legacy backfill holds counts as
//     legacy here until the hold is released.
func (s *service) mayLink(ctx context.Context, product authz.Product, m Media, onBehalfOf uuid.UUID) (bool, error) {
	if m.Purpose == PurposeLegacy || m.DetachExpiryHeld {
		if m.UploadedBy == onBehalfOf {
			return true, nil
		}
		return s.media.HeldBy(ctx, m.ID, product)
	}
	purpose, known := s.catalogue.Lookup(m.Purpose)
	if !known || purpose.OwningProduct() != product {
		return false, nil
	}
	return !slices.Contains(personalPurposes, m.Purpose) || m.UploadedBy == onBehalfOf, nil
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
	if mediaID == uuid.Nil || attachmentID == uuid.Nil {
		return ErrInvalid
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
