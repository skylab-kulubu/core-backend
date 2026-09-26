package media

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
)

// The products a Media attachment's owner_service names.
const (
	// ServiceCore is core itself: the database writes the Media attachments
	// of core's own links. No caller of the service attach API is core.
	ServiceCore = "core"
	// ServiceForms is Skyforms (forms-backend).
	ServiceForms = "forms"
	// ServiceCMS is the CMS (cms-backend).
	ServiceCMS = "cms"
)

// serviceClients maps the Keycloak client of a product's service account to
// the product. A client-credentials token of any other client is no product
// core knows, whatever roles it carries.
//
//   - forms: the confidential client forms-backend requests its service
//     token with (KEYCLOAK_CLIENT_ID); skyforms, the Skyforms login client,
//     in case the service account is set up there.
//   - skycms: the CMS resource client. cms-backend has no service account
//     yet; this is where its attach calls are expected from.
var serviceClients = map[string]string{
	"forms":    ServiceForms,
	"skyforms": ServiceForms,
	"skycms":   ServiceCMS,
}

// The roles another product's records give a Media.
const (
	// RoleAnswer: an Answer file of a Skyforms response or draft.
	RoleAnswer Role = "answer"
	// RoleImage: an image on a CMS page.
	RoleImage Role = "image"
	// RoleFile: a document a CMS page offers.
	RoleFile Role = "file"
)

// serviceRoles are, for each product, the roles its records give a Media and
// the purposes each role accepts. A legacy Media fits every role (the
// transition rule of rolePurposes).
var serviceRoles = map[string]map[Role][]string{
	ServiceForms: {RoleAnswer: {PurposeAnswerFile, PurposeAnswerFileLarge}},
	ServiceCMS:   {RoleImage: {PurposeCMSImage}, RoleFile: {PurposeCMSFile}},
}

// Owner is the record a Media attachment links a Media to: the product that
// owns it, its type there and its id.
type Owner struct {
	Service string    `json:"service"`
	Type    string    `json:"type"`
	ID      uuid.UUID `json:"id"`
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
	if _, known := serviceRoles[product][role]; !known {
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
	if m.Purpose != PurposeLegacy && !slices.Contains(serviceRoles[product][role], m.Purpose) {
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
func (s *service) attachingProduct(p authz.Principal, action authz.Action) (string, error) {
	if !s.authz.Allow(p, authz.Resource{Type: authz.TypeMediaAttachment}, action) {
		return "", ErrAttachForbidden
	}
	product, known := serviceClients[p.Client]
	if !known {
		return "", ErrAttachForbidden
	}
	return product, nil
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
