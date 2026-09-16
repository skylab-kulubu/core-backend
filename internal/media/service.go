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
	created, err := s.media.Create(ctx, Media{
		Name:       name,
		Type:       ctype,
		Size:       int64(len(body)),
		UploadedBy: uploadedBy,
		Kind:       kind,
		Key:        key,
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

func (s *service) withURL(m Media) Media {
	m.URL = publicURL(s.publicBase, m.Key)
	return m
}
