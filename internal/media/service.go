package media

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/lifecycle"
)

type Service interface {
	// Upload stores Media uploaded without a purpose: the legacy Media
	// purpose.
	Upload(ctx context.Context, p authz.Principal, name, contentType string, data []byte) (Media, error)
	// UploadForPurpose stores a file for a Media purpose from the catalogue,
	// under that purpose's rules.
	UploadForPurpose(ctx context.Context, p authz.Principal, purpose string, file UploadedFile) (Media, error)
	Get(ctx context.Context, id uuid.UUID) (Media, error)
	List(ctx context.Context, p authz.Principal) ([]Media, error)
	ListLifecycle(ctx context.Context, p authz.Principal, visibility lifecycle.Visibility) ([]Media, error)
	Delete(ctx context.Context, p authz.Principal, id uuid.UUID) error
	// ArchiveOwn archives an upload the caller made themselves, such as a
	// profile picture they removed. It is the self-service counterpart of
	// Delete: no media-management authority is needed, but only the uploader
	// may release it. The record follows the ordinary archive lifecycle, so
	// the blob stays recoverable until the purge window passes.
	ArchiveOwn(ctx context.Context, p authz.Principal, id uuid.UUID) error
	Restore(ctx context.Context, p authz.Principal, id uuid.UUID) (Media, error)
	// Addresses builds public addresses from the configured base, for
	// records that keep a Media's key, such as a User's profile picture.
	Addresses() Addresses
	// Attach links the Media to a record of the calling product through the
	// service attach API. created is false when the same link already
	// exists; that Media attachment is returned.
	Attach(ctx context.Context, p authz.Principal, mediaID uuid.UUID, req AttachRequest) (a Attachment, created bool, err error)
	// Detach removes a Media attachment of the calling product; removing
	// one that is not there succeeds.
	Detach(ctx context.Context, p authz.Principal, mediaID, attachmentID uuid.UUID) error
}

type service struct {
	media              Store
	blobs              BlobStore
	authz              authz.Authorizer
	addresses          Addresses
	uploadStagingGrace time.Duration
	catalogue          Catalogue
	decoding           *DecodeBudget
	serviceProducts    []authz.Product
}

func NewService(media Store, blobs BlobStore, az authz.Authorizer, publicBase string) Service {
	return NewServiceWithOptions(media, blobs, az, publicBase, ServiceOptions{})
}

type ServiceOptions struct {
	UploadStagingGrace time.Duration
	// Catalogue is the Media purpose catalogue. The zero value is the
	// reviewed catalogue carried in the binary.
	Catalogue Catalogue
	// ImageAddressMode is where image sizes are served from
	// (MEDIA_IMAGE_ADDRESS_MODE); empty is AddressStoredSizes.
	ImageAddressMode AddressMode
	// DecodeBudget is the process's decode budget, shared with the
	// backfills. Nil makes one for this service alone.
	DecodeBudget *DecodeBudget
	// ServiceProducts are the products with a service client configured
	// (authz.ServiceClients): only they can attach Media, so a service
	// purpose of any other product cannot be uploaded.
	ServiceProducts []authz.Product
}

func NewServiceWithOptions(media Store, blobs BlobStore, az authz.Authorizer, publicBase string, options ServiceOptions) Service {
	grace := options.UploadStagingGrace
	if grace <= 0 {
		grace = DefaultUploadStagingGrace
	}
	catalogue := options.Catalogue
	if catalogue.purposes == nil {
		catalogue = reviewedCatalogue()
	}
	addresses := Addresses{Base: publicBase, Mode: options.ImageAddressMode}
	decoding := options.DecodeBudget
	if decoding == nil {
		decoding = NewDecodeBudget(DecodeBudgetConfig{})
	}
	return &service{
		media: media, blobs: blobs, authz: az, addresses: addresses, uploadStagingGrace: grace, catalogue: catalogue,
		decoding: decoding, serviceProducts: options.ServiceProducts,
	}
}

// reviewedCatalogue is the catalogue carried in the binary. Core validates it
// at startup (LoadCatalogue) before any service exists, so a failure here is
// a build that never passed its own tests.
func reviewedCatalogue() Catalogue {
	catalogue, err := LoadCatalogue()
	if err != nil {
		panic(err)
	}
	return catalogue
}

// UploadedFile is a file as its client sent it: its name, the type the client
// declared, and its bytes.
type UploadedFile struct {
	Name        string
	ContentType string
	Data        []byte
}

func (s *service) Upload(ctx context.Context, p authz.Principal, name, contentType string, data []byte) (Media, error) {
	legacy, _ := s.catalogue.Lookup(PurposeLegacy) // every catalogue has it
	return s.upload(ctx, p, legacy, UploadedFile{Name: name, ContentType: contentType, Data: data})
}

// storedFile is what an uploaded file becomes once its purpose's rules
// accept it.
type storedFile struct {
	body      []byte
	ctype     string
	kind      string
	keyPrefix string
	// keySuffix ends the object key: .svg for an SVG.
	keySuffix string
	// image is set for an image core encoded or sanitized itself: its
	// size, its sizes to store beside it and its cover colours. Nil for
	// anything else, whose sizes core does not make.
	image *reencodedImage
}

// imageFile is the storedFile of an image core encoded or sanitized.
func imageFile(img reencodedImage) storedFile {
	file := storedFile{body: img.body, ctype: img.ctype, kind: KindImage, keyPrefix: "images/", image: &img}
	if img.ctype == svgType {
		file.keySuffix = ".svg"
	}
	return file
}

func (s *service) UploadForPurpose(ctx context.Context, p authz.Principal, purposeName string, file UploadedFile) (Media, error) {
	purpose, ok := s.catalogue.Lookup(purposeName)
	if !ok || purposeName == PurposeLegacy {
		// legacy is internal: only Upload, for Media uploaded without a
		// purpose, stores it.
		return Media{}, &PurposeRefusal{Err: ErrPurposeUnknown, Purpose: purposeName}
	}
	return s.upload(ctx, p, purpose, file)
}

func (s *service) upload(ctx context.Context, p authz.Principal, purpose Purpose, file UploadedFile) (Media, error) {
	if !s.authz.Allow(p, authz.Resource{Type: authz.TypeMedia, MediaUploader: purpose.Uploader}, authz.Upload) {
		return Media{}, &PurposeRefusal{Err: ErrPurposeForbidden, Purpose: purpose.Name}
	}
	if purpose.Visibility == VisibilityPrivate {
		// Private Media storage (encryption, the private bucket) is not
		// built yet. Until it is, a private purpose is refused, never stored
		// publicly instead.
		return Media{}, &PurposeRefusal{Err: ErrPrivateMediaDisabled, Purpose: purpose.Name}
	}
	if purpose.Transport == TransportDirect {
		return Media{}, &PurposeRefusal{Err: ErrDirectUploadOnly, Purpose: purpose.Name}
	}
	if !s.attachable(purpose) {
		return Media{}, &PurposeRefusal{Err: ErrPurposeNotAvailable, Purpose: purpose.Name}
	}
	if len(file.Data) == 0 {
		return Media{}, ErrInvalid
	}
	uploadedBy, err := uuid.Parse(p.ID)
	if err != nil {
		return Media{}, ErrInvalid
	}
	stored, err := s.storedFile(ctx, purpose, file)
	if err != nil {
		return Media{}, err
	}
	key := stored.keyPrefix + uuid.NewString() + stored.keySuffix

	staging, durableStaging := s.media.(UploadStagingStore)
	if durableStaging {
		if err := staging.StageUpload(ctx, key, uploadedBy, time.Now().UTC().Add(s.uploadStagingGrace)); err != nil {
			return Media{}, err
		}
	}
	operationTimeout := s.uploadStagingGrace / 2
	if operationTimeout > 10*time.Minute {
		operationTimeout = 10 * time.Minute
	}
	operationCtx, cancelOperation := context.WithTimeout(ctx, operationTimeout)
	defer cancelOperation()
	serving := ServingMetadata(stored.ctype, file.Name)
	if err := s.blobs.Put(operationCtx, key, stored.body, serving); err != nil {
		return Media{}, s.cleanupRejectedUpload(ctx, staging, durableStaging, key, nil, err)
	}
	var sizes []string
	var shown ImageSize
	var sizeObjects map[string]SizeObject
	colors, colorsComputed := []string{}, false
	if stored.image != nil {
		if len(stored.image.sizes) > 0 && !canHaveSizeObjects(key) {
			return Media{}, s.cleanupRejectedUpload(ctx, staging, durableStaging, key, nil, fmt.Errorf("media: key %q cannot have sizes", key))
		}
		for _, size := range stored.image.sizes {
			sizes = append(sizes, sizeObjectKey(key, size.name, size.ctype))
			if err := s.blobs.Put(operationCtx, sizes[len(sizes)-1], size.body, ServingMetadata(size.ctype, file.Name)); err != nil {
				return Media{}, s.cleanupRejectedUpload(ctx, staging, durableStaging, key, sizes, err)
			}
		}
		shown, sizeObjects = stored.image.size, sizeObjectsOf(stored.image.sizes)
		colors, colorsComputed = stored.image.coverColors, true
	} else if stored.kind == KindImage {
		colors, colorsComputed = s.coverColors(stored.body)
	}
	item := Media{
		Name:                file.Name,
		Type:                stored.ctype,
		Size:                int64(len(stored.body)),
		UploadedBy:          uploadedBy,
		Kind:                stored.kind,
		Width:               shown.Width,
		Height:              shown.Height,
		SizeObjects:         sizeObjects,
		Purpose:             purpose.Name,
		ExpiresAt:           pendingExpiry(purpose, time.Now().UTC()),
		Key:                 key,
		CoverColors:         colors,
		CoverColorsComputed: colorsComputed,

		ServingPolicyApplied: true,
	}
	var created Media
	if durableStaging {
		created, err = staging.CreateStaged(operationCtx, item)
	} else {
		created, err = s.media.Create(operationCtx, item)
	}
	if err != nil {
		if errors.Is(err, ErrPublicationUncertain) {
			// The metadata commit may have succeeded. Never delete an object that
			// may already be referenced; an uncommitted attempt retains its
			// durable staging row for the sweeper.
			return Media{}, err
		}
		return Media{}, s.cleanupRejectedUpload(ctx, staging, durableStaging, key, sizes, err)
	}
	return s.withURL(created), nil
}

// storedFile is what the purpose's rules make of the file. An image for a
// purpose is decoded within the decode budget, and an SVG sanitized within
// its own slot too; a wait that runs out is ErrDecodeBusy. A file uploaded
// without a purpose is never decoded here.
func (s *service) storedFile(ctx context.Context, purpose Purpose, file UploadedFile) (storedFile, error) {
	if purpose.LegacyRules {
		if detectContentType(file.Data) == svgType && len(file.Data) <= maxImageBytes {
			return s.legacySVG(ctx, file.Data)
		}
		return legacyFile(file)
	}
	if isImage(file.Data) {
		acquire := s.decoding.Acquire
		if isSVG(file.Data) && purpose.accepts(svgType) {
			acquire = s.decoding.AcquireSVG
		}
		release, err := acquire(ctx)
		if err != nil {
			return storedFile{}, err
		}
		defer release()
	}
	return purposeFile(purpose, file.Data)
}

// legacySVG stores an SVG uploaded without a purpose the way a purpose
// stores one: sanitized, under a .svg key, within the SVG decoding slot.
// Media uploaded without a purpose keep accepting what they accepted, so
// an SVG the sanitizer refuses is stored anyway, as an opaque download
// (application/octet-stream, attachment) that never renders.
func (s *service) legacySVG(ctx context.Context, data []byte) (storedFile, error) {
	release, err := s.decoding.AcquireSVG(ctx)
	if err != nil {
		return storedFile{}, err
	}
	defer release()
	clean, err := sanitizeSVG(data, MaxImageDimension)
	if err != nil {
		return storedFile{body: data, ctype: "application/octet-stream", kind: KindFile, keyPrefix: "files/"}, nil
	}
	return imageFile(reencodedImage{body: clean, ctype: svgType, coverColors: []string{}}), nil
}

// coverColors picks the cover colours of an image stored as uploaded,
// within the decode budget. They are decoration: when no decoding slot is
// free the upload does not wait; the image is stored without them
// (computed false), and the cover colour backfill picks them later.
func (s *service) coverColors(data []byte) ([]string, bool) {
	release, ok := s.decoding.TryAcquire()
	if !ok {
		return []string{}, false
	}
	defer release()
	return ExtractCoverColors(data), true
}

// attachable reports whether something can attach Media of the purpose
// today: core, or the purpose's product once it has a service client. A
// Media nothing can attach would only wait for its expiry.
func (s *service) attachable(purpose Purpose) bool {
	return purpose.Attach == AttachCore || (purpose.Service != "" && slices.Contains(s.serviceProducts, purpose.Service))
}

// pendingExpiry is when a Media of the purpose uploaded at now is purged if
// no Media attachment links it by then. A purpose without a pending TTL
// (legacy) keeps it.
func pendingExpiry(purpose Purpose, now time.Time) *time.Time {
	if purpose.PendingTTL <= 0 {
		return nil
	}
	expires := now.Add(purpose.PendingTTL)
	return &expires
}

// legacyFile applies the rules Media uploaded without a purpose had before
// Media purpose: a raster image up to 10 MiB, a PDF named .pdf up to 20
// MiB, or any other named file up to 20 MiB, served as a download. A raster
// image keeps its own bytes, stripped of metadata (sanitizeImage), and gets
// no sizes. An SVG up to 10 MiB is stored by legacySVG.
func legacyFile(file UploadedFile) (storedFile, error) {
	name, contentType, data := file.Name, file.ContentType, file.Data
	if strings.HasPrefix(contentType, "image/") || isImage(data) {
		if len(data) > maxImageBytes {
			return storedFile{}, ErrInvalid
		}
		clean, detected, err := sanitizeImage(data)
		if err != nil {
			return storedFile{}, err
		}
		return storedFile{body: clean, ctype: detected, kind: KindImage, keyPrefix: "images/"}, nil
	}
	if strings.EqualFold(strings.TrimSpace(contentType), pdfType) || isPDF(data) {
		if len(data) > maxFileBytes || !isPDF(data) {
			return storedFile{}, ErrInvalid
		}
		if ext, err := fileExtension(name); err != nil || ext != "pdf" {
			return storedFile{}, ErrInvalid
		}
		return storedFile{body: data, ctype: pdfType, kind: KindFile, keyPrefix: "files/"}, nil
	}
	if len(data) > maxFileBytes {
		return storedFile{}, ErrInvalid
	}
	if _, err := fileExtension(name); err != nil {
		return storedFile{}, err
	}
	return storedFile{body: data, ctype: contentType, kind: KindFile, keyPrefix: "files/"}, nil
}

// purposeFile accepts a file by its content under its purpose's rules. The
// name and the declared type play no part. The caller holds a decoding
// slot for an image.
func purposeFile(purpose Purpose, data []byte) (storedFile, error) {
	if int64(len(data)) > purpose.MaxBytes {
		return storedFile{}, &PurposeRefusal{Err: ErrTooLarge, Purpose: purpose.Name, MaxBytes: purpose.MaxBytes}
	}
	detected := detectContentType(data)
	if detected == "" || !purpose.accepts(detected) {
		return storedFile{}, purpose.typeRefusal()
	}
	if detected == pdfType {
		return storedFile{body: data, ctype: pdfType, kind: KindFile, keyPrefix: "files/"}, nil
	}
	var img reencodedImage
	var err error
	switch {
	case detected == svgType:
		var clean []byte
		clean, err = sanitizeSVG(data, purpose.Image.maxDimension())
		if errors.Is(err, errSVGTooLarge) {
			return storedFile{}, &PurposeRefusal{Err: ErrTooLarge, Purpose: purpose.Name, MaxBytes: maxSVGBytes}
		}
		// Stored as SVG, sanitized; no sizes (every size is the SVG itself)
		// and no cover colours.
		img = reencodedImage{body: clean, ctype: svgType, coverColors: []string{}}
	case purpose.Image.Reencode && detected == "image/gif":
		img, err = reencodeGIF(data, purpose.Image)
	case purpose.Image.Reencode && isAnimatedWebP(data):
		// Go has no WebP encoder: an animated WebP is kept as uploaded
		// once its structure is checked, without metadata and sizes.
		var clean []byte
		var canvas ImageSize
		clean, canvas, err = cleanAnimatedWebP(data, purpose.Image.maxDimension())
		img = reencodedImage{body: clean, ctype: "image/webp", size: canvas, coverColors: []string{}}
	case purpose.Image.Reencode:
		img, err = reencodeRaster(data, purpose.Image)
	default:
		clean, ctype, err := sanitizeImage(data)
		if err != nil {
			return storedFile{}, err
		}
		return storedFile{body: clean, ctype: ctype, kind: KindImage, keyPrefix: "images/"}, nil
	}
	var tooLarge errImageTooLarge
	switch {
	case errors.As(err, &tooLarge):
		return storedFile{}, &PurposeRefusal{Err: ErrImageTooLarge, Purpose: purpose.Name, MaxPixels: tooLarge.maxPixels}
	case errors.Is(err, ErrInvalid), errors.Is(err, errSVGRefused):
		// The content starts like an accepted type but is not a valid
		// image of it, or an SVG core does not sanitize.
		return storedFile{}, purpose.typeRefusal()
	case err != nil:
		return storedFile{}, err
	}
	return imageFile(img), nil
}

// cleanupRejectedUpload deletes what a refused upload wrote: the stored
// sizes written so far (sizes), then the object at key. The staging sweeper
// finds any of them a failure here leaves.
func (s *service) cleanupRejectedUpload(ctx context.Context, staging UploadStagingStore, durableStaging bool, key string, sizes []string, cause error) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if durableStaging {
		if err := staging.ReadyStagedUploadForCleanup(cleanupCtx, key, time.Now().UTC()); err != nil {
			cause = errors.Join(cause, fmt.Errorf("schedule rejected upload cleanup: %w", err))
		}
	}
	for _, size := range sizes {
		if err := s.blobs.Delete(cleanupCtx, size); err != nil {
			return errors.Join(cause, fmt.Errorf("cleanup rejected upload size: %w", err))
		}
	}
	if err := s.blobs.Delete(cleanupCtx, key); err != nil {
		// A durable staging row is deliberately retained for the sweeper.
		return errors.Join(cause, fmt.Errorf("cleanup rejected upload blob: %w", err))
	}
	if durableStaging {
		if err := staging.CancelStagedUpload(cleanupCtx, key); err != nil {
			// The object is already absent; retaining the intent is safe and the
			// idempotent sweeper will remove it later.
			return errors.Join(cause, fmt.Errorf("complete rejected upload cleanup: %w", err))
		}
	}
	return cause
}

func (s *service) Get(ctx context.Context, id uuid.UUID) (Media, error) {
	if !s.authz.Allow(authz.Principal{}, authz.Resource{Type: authz.TypeMedia}, authz.Read) {
		return Media{}, ErrForbidden
	}
	m, err := s.media.Get(ctx, id)
	if err != nil {
		return Media{}, err
	}
	return s.withURL(m), nil
}

func (s *service) List(ctx context.Context, p authz.Principal) ([]Media, error) {
	return s.ListLifecycle(ctx, p, lifecycle.CurrentOnly)
}

func (s *service) ListLifecycle(ctx context.Context, p authz.Principal, visibility lifecycle.Visibility) ([]Media, error) {
	if !s.authz.Allow(p, authz.Resource{Type: authz.TypeMedia}, authz.List) {
		return nil, ErrForbidden
	}
	items, err := s.media.ListLifecycle(ctx, visibility)
	if err != nil {
		return nil, err
	}
	out := make([]Media, 0, len(items))
	for _, m := range items {
		out = append(out, s.withURL(m))
	}
	return out, nil
}

func (s *service) Delete(ctx context.Context, p authz.Principal, id uuid.UUID) error {
	if !s.authz.Allow(p, authz.Resource{Type: authz.TypeMedia}, authz.Delete) {
		return ErrForbidden
	}
	if _, err := s.media.GetIncludingDeleted(ctx, id); err != nil {
		return err
	}
	return s.media.Archive(ctx, id, lifecycle.ActorID(p.ID))
}

func (s *service) ArchiveOwn(ctx context.Context, p authz.Principal, id uuid.UUID) error {
	uploader := lifecycle.ActorID(p.ID)
	if uploader == nil {
		return ErrInvalid
	}
	m, err := s.media.GetIncludingDeleted(ctx, id)
	if err != nil {
		return err
	}
	if m.UploadedBy != *uploader {
		return ErrForbidden
	}
	return s.media.Archive(ctx, id, uploader)
}

func (s *service) Restore(ctx context.Context, p authz.Principal, id uuid.UUID) (Media, error) {
	if !s.authz.Allow(p, authz.Resource{Type: authz.TypeMedia}, authz.Delete) {
		return Media{}, ErrForbidden
	}
	m, err := s.media.GetIncludingDeleted(ctx, id)
	if err != nil {
		return Media{}, err
	}
	if err := s.media.Restore(ctx, id); err != nil {
		return Media{}, err
	}
	if m.DeletedAt != nil {
		// Restore left the Media with no expiry; its purpose's window starts
		// again now. Should this step fail, the Media is only kept longer.
		purpose, _ := s.catalogue.Lookup(m.Purpose)
		if err := s.media.ExpireUnattachedAt(ctx, id, pendingExpiry(purpose, time.Now().UTC())); err != nil {
			return Media{}, err
		}
	}
	return s.Get(ctx, id)
}

func (s *service) Addresses() Addresses {
	return s.addresses
}

func (s *service) withURL(m Media) Media {
	m.Sizes = nil
	if m.BlobPurgeStartedAt != nil || m.BlobPurgedAt != nil {
		m.URL = ""
		return m
	}
	m.URL = s.addresses.Object(m.Key)
	purpose, _ := s.catalogue.Lookup(m.Purpose)
	m.Sizes = s.addresses.imageAddresses(m, purpose.Image.Sizes)
	return m
}
