package media

import (
	"context"
	"strings"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
)

type Service interface {
	Upload(ctx context.Context, p authz.Principal, name, contentType string, data []byte) (Media, error)
	Get(ctx context.Context, id uuid.UUID) (Media, error)
	List(ctx context.Context, p authz.Principal) ([]Media, error)
	Delete(ctx context.Context, p authz.Principal, id uuid.UUID) error
}

type service struct {
	media      Store
	blobs      BlobStore
	authz      authz.Authorizer
	publicBase string
}

func NewService(media Store, blobs BlobStore, az authz.Authorizer, publicBase string) Service {
	return &service{media: media, blobs: blobs, authz: az, publicBase: publicBase}
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
	if strings.HasPrefix(contentType, "image/") || isJPEG(data) || isPNG(data) || isWebP(data) || isGIF(data) || isSVG(data) {
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
	} else if strings.EqualFold(strings.TrimSpace(contentType), "application/pdf") || isPDF(data) {
		if len(data) > maxFileBytes || !isPDF(data) {
			return Media{}, ErrInvalid
		}
		if ext, err := fileExtension(name); err != nil || ext != "pdf" {
			return Media{}, ErrInvalid
		}
		body = data
		ctype = "application/pdf"
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

	if err := s.blobs.Put(ctx, key, body, ctype); err != nil {
		return Media{}, err
	}
	colors := []string{}
	colorsComputed := false
	if kind == KindImage {
		colors = ExtractCoverColors(body)
		colorsComputed = true
	}
	created, err := s.media.Create(ctx, Media{
		Name:                name,
		Type:                ctype,
		Size:                int64(len(body)),
		UploadedBy:          uploadedBy,
		Kind:                kind,
		Key:                 key,
		CoverColors:         colors,
		CoverColorsComputed: colorsComputed,
	})
	if err != nil {
		return Media{}, err
	}
	return s.withURL(created), nil
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
	if !s.authz.Allow(p, authz.Resource{Type: authz.TypeMedia}, authz.List) {
		return nil, ErrForbidden
	}
	items, err := s.media.List(ctx)
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
	m, err := s.media.Delete(ctx, id)
	if err != nil {
		return err
	}
	return s.blobs.Delete(ctx, m.Key)
}

func (s *service) withURL(m Media) Media {
	if strings.TrimSpace(s.publicBase) == "" {
		m.URL = m.Key
		return m
	}
	m.URL = PublicURL(s.publicBase, m.Key)
	return m
}
