package media

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/lifecycle"
)

type Service interface {
	Upload(ctx context.Context, p authz.Principal, name, contentType string, data []byte) (Media, error)
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
}

type service struct {
	media              Store
	blobs              BlobStore
	authz              authz.Authorizer
	publicBase         string
	uploadStagingGrace time.Duration
}

func NewService(media Store, blobs BlobStore, az authz.Authorizer, publicBase string) Service {
	return NewServiceWithOptions(media, blobs, az, publicBase, ServiceOptions{})
}

type ServiceOptions struct {
	UploadStagingGrace time.Duration
}

func NewServiceWithOptions(media Store, blobs BlobStore, az authz.Authorizer, publicBase string, options ServiceOptions) Service {
	grace := options.UploadStagingGrace
	if grace <= 0 {
		grace = DefaultUploadStagingGrace
	}
	return &service{media: media, blobs: blobs, authz: az, publicBase: publicBase, uploadStagingGrace: grace}
}

func (s *service) Upload(ctx context.Context, p authz.Principal, name, contentType string, data []byte) (Media, error) {
	if !s.authz.Allow(p, authz.Resource{Type: authz.TypeMedia}, authz.Upload) {
		return Media{}, ErrForbidden
	}
	if len(data) == 0 {
		return Media{}, ErrInvalid
	}
	uploadedBy, err := uuid.Parse(p.ID)
	if err != nil {
		return Media{}, ErrInvalid
	}

	var (
		key   string
		ctype string
		kind  string
		body  []byte
	)
	if strings.HasPrefix(contentType, "image/") || isImage(data) {
		if len(data) > maxImageBytes {
			return Media{}, ErrInvalid
		}
		clean, detected, err := sanitizeImage(data)
		if err != nil {
			return Media{}, err
		}
		body = clean
		ctype = detected
		kind = KindImage
		key = "images/" + uuid.NewString()
	} else if strings.EqualFold(strings.TrimSpace(contentType), pdfType) || isPDF(data) {
		if len(data) > maxFileBytes || !isPDF(data) {
			return Media{}, ErrInvalid
		}
		if ext, err := fileExtension(name); err != nil || ext != "pdf" {
			return Media{}, ErrInvalid
		}
		body = data
		ctype = pdfType
		kind = KindFile
		key = "files/" + uuid.NewString()
	} else {
		if len(data) > maxFileBytes {
			return Media{}, ErrInvalid
		}
		if _, err := fileExtension(name); err != nil {
			return Media{}, err
		}
		body = data
		ctype = contentType
		kind = KindFile
		key = "files/" + uuid.NewString()
	}

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
	serving := ServingMetadata(ctype, name)
	if err := s.blobs.Put(operationCtx, key, body, serving); err != nil {
		return Media{}, s.cleanupRejectedUpload(ctx, staging, durableStaging, key, err)
	}
	colors := []string{}
	colorsComputed := false
	if kind == KindImage {
		colors = ExtractCoverColors(body)
		colorsComputed = true
	}
	item := Media{
		Name:                name,
		Type:                ctype,
		Size:                int64(len(body)),
		UploadedBy:          uploadedBy,
		Kind:                kind,
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
		return Media{}, s.cleanupRejectedUpload(ctx, staging, durableStaging, key, err)
	}
	return s.withURL(created), nil
}

func (s *service) cleanupRejectedUpload(ctx context.Context, staging UploadStagingStore, durableStaging bool, key string, cause error) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if durableStaging {
		if err := staging.ReadyStagedUploadForCleanup(cleanupCtx, key, time.Now().UTC()); err != nil {
			cause = errors.Join(cause, fmt.Errorf("schedule rejected upload cleanup: %w", err))
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
	if _, err := s.media.GetIncludingDeleted(ctx, id); err != nil {
		return Media{}, err
	}
	if err := s.media.Restore(ctx, id); err != nil {
		return Media{}, err
	}
	return s.Get(ctx, id)
}

func (s *service) withURL(m Media) Media {
	if m.BlobPurgeStartedAt != nil || m.BlobPurgedAt != nil {
		m.URL = ""
		return m
	}
	if strings.TrimSpace(s.publicBase) == "" {
		m.URL = m.Key
		return m
	}
	m.URL = PublicURL(s.publicBase, m.Key)
	return m
}
