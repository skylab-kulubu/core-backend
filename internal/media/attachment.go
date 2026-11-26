package media

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
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
	// RoleEventFile is one of an Event's downloadable files (event_files,
	// migration 20260928160000).
	RoleEventFile Role = "event_file"
	// RoleEventVideo is one of an Event's videos (event_videos).
	RoleEventVideo Role = "event_video"
	// RoleEventVideoPoster is the poster of one of an Event's videos
	// (event_videos.poster_media_id, migration 20260929140000). The Event
	// owns the link, as it owns its cover's.
	RoleEventVideoPoster Role = "event_video_poster"
	// RoleEventVideoFrame is the frame core took of one of an Event's
	// videos (event_videos.frame_media_id, migration 20260929160000): the
	// video's poster while its organizers uploaded none. The Event owns the
	// link, as it owns the poster's.
	RoleEventVideoFrame Role = "event_video_frame"
	// RoleEventMap is the map of an Event's place, which core draws itself
	// (events.map_media_id, migration 20261122160000). Copies of an Event
	// with the same point share it.
	RoleEventMap Role = "event_map"
	// RoleEventTicketImage is the image an Event's PDF ticket shows in its
	// band instead of its cover (event_ticket_images.media_id, migration
	// 20261127190000; docs/ticket-image.md): an Event image, as a video's
	// poster. The Event owns the link.
	RoleEventTicketImage Role = "event_ticket_image"
	// RoleCertificateFont is the checked file of a team's or the club's
	// certificate font (certificate_fonts.media_id, migration
	// 20261127183000; docs/certificate-fonts.md). The font owns the link;
	// a layout that uses it links the same Media as a certificate asset.
	RoleCertificateFont Role = "certificate_font"
)

// The roles another product's records give a Media, through the service
// attach API.
const (
	// RoleFormsAnswer: an Answer file of a Skyforms response or draft.
	RoleFormsAnswer Role = "answer"
	// RoleCMSImage: an image on a CMS page, block or collection item.
	RoleCMSImage Role = "image"
	// RoleCMSFile: a document a CMS page offers.
	RoleCMSFile Role = "file"
)

// rolePurposes are, for each product, the roles its records give a Media and
// the Media purposes each role accepts, the role's own purpose first (the
// legacy backfill gives that one). The database keeps a copy for its
// backstop (media_role_purposes, migration 20260926161000); a test keeps
// the two equal. The two Event purposes fit both Event roles and a video's
// poster: the organizer's picker offers every photo of the team's Events for
// the cover, the gallery and a poster alike. A profile picture is linked
// only by POST /v1/users/me/profile-picture, which uploads it as
// profile_picture, so no link checks that role; the legacy backfill reads
// it.
//
// Transition rule: a legacy Media fits every role but those of
// rolesWithoutLegacy, as any Media could be linked anywhere before Media
// purpose (fits). superadmin, Skyforms and CMS still upload without a
// purpose; the rule ends when they send one (media redesign tickets 09 and
// 15).
var rolePurposes = map[authz.Product]map[Role][]string{
	authz.ProductCore: {
		RoleEventCover:     {PurposeEventCover, PurposeEventGallery},
		RoleEventGallery:   {PurposeEventGallery, PurposeEventCover},
		RoleProfilePicture: {PurposeProfilePicture},
		// A layout links its elements' fonts (fontMediaId) beside its
		// images, all in this role: the database writes one role per
		// layout column (certificate_layout_media_ids).
		RoleCertificateAsset: {PurposeCertificateAsset, PurposeCertificateFont},
		RoleEventFile:        {PurposeClubFile},
		RoleEventVideo:       {PurposeVideo},
		RoleEventVideoPoster: {PurposeEventCover, PurposeEventGallery},
		RoleEventVideoFrame:  {PurposeVideoFrame},
		RoleEventMap:         {PurposeEventMap},
		RoleEventTicketImage: {PurposeEventCover, PurposeEventGallery},
		RoleCertificateFont:  {PurposeCertificateFont},
	},
	authz.ProductForms: {RoleFormsAnswer: {PurposeAnswerFile, PurposeAnswerFileLarge, PurposeAnswerFileGuest}},
	authz.ProductCMS:   {RoleCMSImage: {PurposeCMSImage}, RoleCMSFile: {PurposeCMSFile}},
}

// rolesWithoutLegacy are the roles the transition rule leaves out: a legacy
// Media does not fit them. They were made after Media purpose, so no Media
// was ever linked in them without one: an Event's files and videos, whose
// purposes are sent by Direct upload (club files also scanned), which a
// legacy upload never was, a video's poster, a video's frame, an Event's
// map (which core makes itself), its ticket image and a certificate font
// (checked by pdf-tools first). The database keeps a copy
// (media_roles_without_legacy, migrations 20260928160000, 20260929140000,
// 20260929160000, 20261122160000, 20261127183000 and 20261127190000); a
// test keeps the two equal.
var rolesWithoutLegacy = map[authz.Product][]Role{
	authz.ProductCore: {RoleEventFile, RoleEventVideo, RoleEventVideoPoster, RoleEventVideoFrame, RoleEventMap, RoleEventTicketImage, RoleCertificateFont},
}

// fits reports whether a Media of the purpose may play the product's role.
func fits(product authz.Product, role Role, purpose string) bool {
	if purpose == PurposeLegacy {
		return !slices.Contains(rolesWithoutLegacy[product], role)
	}
	return slices.Contains(rolePurposes[product][role], purpose)
}

// Link refusals by the Media itself. Each also matches ErrInvalid.
var (
	// ErrPurposeMismatch: the Media's purpose does not fit the role, such as
	// a PDF uploaded for a CMS page linked as an Event cover.
	ErrPurposeMismatch = fmt.Errorf("media: its purpose does not fit the role: %w", ErrInvalid)
	// ErrNotLinkable: there is no such Media, or it is archived, its blob is
	// purged or being purged, or it expired unattached.
	ErrNotLinkable = fmt.Errorf("media: cannot be linked: %w", ErrInvalid)
)

// LinkRefusal is a link refused for its Media: by the Media's own rules, or
// by a rule of the record's domain (such as the Team media library). errors.Is
// matches its Err.
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
	_, err := checkLink(ctx, l.store, id, authz.ProductCore, role, nil)
	return err
}

// checkLink is the one check of every new link, core's own and another
// product's: the Media exists and is linkable, the product may link it at
// all (may, when the product has such a rule), and its purpose fits the
// product's role. A Media the product may not link is refused exactly like
// one that does not exist, so the refusal tells the caller nothing about it.
// The Media it read comes back with a purpose refusal.
func checkLink(ctx context.Context, store Store, id uuid.UUID, product authz.Product, role Role, may func(Media) (bool, error)) (Media, error) {
	notLinkable := &LinkRefusal{Err: ErrNotLinkable, MediaID: id, Role: role}
	m, err := store.GetIncludingDeleted(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return Media{}, notLinkable
	}
	if err != nil {
		return Media{}, err
	}
	if !linkable(m, time.Now()) {
		return Media{}, notLinkable
	}
	if may != nil {
		allowed, err := may(m)
		if err != nil {
			return Media{}, err
		}
		if !allowed {
			return Media{}, notLinkable
		}
	}
	if !fits(product, role, m.Purpose) {
		return m, &LinkRefusal{Err: ErrPurposeMismatch, MediaID: id, Role: role, Purpose: m.Purpose}
	}
	return m, nil
}

// linkable reports whether m may get a new Media attachment at now: it is
// not archived, no purge has started, and it has not expired. A Media removed
// from a record can be linked again until its window ends.
func linkable(m Media, now time.Time) bool {
	if m.DeletedAt != nil || m.BlobPurgeStartedAt != nil || m.BlobPurgedAt != nil {
		return false
	}
	return !m.expired(now)
}
