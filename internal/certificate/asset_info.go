package certificate

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

// AssetInfo is a certificate asset's size as the template editor needs it
// (GET /v1/certificate-assets/{id}/info, docs/certificate-template-errors.md):
// a PDF's first page's visible box and an image's natural size, in layout
// px (a PDF's pt × 96/72), with a PDF's page and text counts.
type AssetInfo struct {
	ContentType string  `json:"contentType"`
	WidthPx     float64 `json:"widthPx"`
	HeightPx    float64 `json:"heightPx"`
	Pages       int     `json:"pages"`
	TextCount   int     `json:"textCount"`
}

// AssetPreview is what the editor draws a background with (GET
// /v1/certificate-assets/{id}/preview): a PDF's page as a PNG, an image as
// itself.
type AssetPreview struct {
	ContentType string
	Data        []byte
}

// The long side of an asset preview: the default and its bounds (pdf-tools'
// /v1/render takes 16–4000).
const (
	DefaultAssetPreviewPx = 2000
	MinAssetPreviewPx     = 16
	MaxAssetPreviewPx     = 4000
)

// AssetOrigin is who uploaded an asset's Media and for which purpose.
type AssetOrigin struct {
	UploadedBy uuid.UUID
	Purpose    string
	// Name is the Media's file name.
	Name string
}

// assetOrigins says where an asset's Media came from; MediaAssets does.
type assetOrigins interface {
	AssetOrigin(ctx context.Context, id uuid.UUID) (AssetOrigin, error)
}

// templatesUsingMedia finds the current templates whose draft or a
// published version links a Media; PostgresStore and MemoryStore do.
type templatesUsingMedia interface {
	TemplatesUsingMedia(ctx context.Context, mediaID uuid.UUID) ([]Template, error)
}

func (m MediaAssets) AssetOrigin(ctx context.Context, id uuid.UUID) (AssetOrigin, error) {
	if m.Media == nil {
		return AssetOrigin{}, ErrInvalid
	}
	metadata, err := m.Media.Get(ctx, id)
	if errors.Is(err, media.ErrNotFound) {
		return AssetOrigin{}, ErrNotFound
	}
	if err != nil {
		return AssetOrigin{}, err
	}
	return AssetOrigin{UploadedBy: metadata.UploadedBy, Purpose: metadata.Purpose, Name: metadata.Name}, nil
}

func (s *service) AssetInfo(ctx context.Context, p authz.Principal, id uuid.UUID) (AssetInfo, error) {
	asset, err := s.allowedAsset(ctx, p, id)
	if err != nil {
		return AssetInfo{}, err
	}
	contentType := mediaType(asset.ContentType)
	if isPDF(contentType) {
		info, err := s.pdfInfo(ctx, asset.Data)
		if err != nil {
			return AssetInfo{}, err
		}
		if info.Encrypted {
			return AssetInfo{}, invalid(CodeBackgroundPDFEncrypted)
		}
		return AssetInfo{ContentType: contentType, WidthPx: info.WidthPt * pxPerPt, HeightPx: info.HeightPt * pxPerPt, Pages: info.Pages, TextCount: info.TextCount}, nil
	}
	if !strings.HasPrefix(contentType, "image/") {
		return AssetInfo{}, invalid(CodeAssetTypeUnsupported)
	}
	w, h, err := backgroundSize(contentType, asset.Data)
	if err != nil {
		return AssetInfo{}, err
	}
	return AssetInfo{ContentType: contentType, WidthPx: w, HeightPx: h, Pages: 1}, nil
}

func (s *service) AssetPreview(ctx context.Context, p authz.Principal, id uuid.UUID, maxPx int) (AssetPreview, error) {
	if maxPx < MinAssetPreviewPx || maxPx > MaxAssetPreviewPx {
		return AssetPreview{}, ErrInvalid
	}
	asset, err := s.allowedAsset(ctx, p, id)
	if err != nil {
		return AssetPreview{}, err
	}
	contentType := mediaType(asset.ContentType)
	if !isPDF(contentType) {
		if !strings.HasPrefix(contentType, "image/") {
			return AssetPreview{}, invalid(CodeAssetTypeUnsupported)
		}
		return AssetPreview{ContentType: contentType, Data: asset.Data}, nil
	}
	if s.pdfTools == nil {
		return AssetPreview{}, pdfToolsUnavailable(nil)
	}
	if err := checkBackgroundPDF(asset.Data); err != nil {
		return AssetPreview{}, err
	}
	// The editor asks for the same page again and again; pdf-tools has two
	// workers, so a drawn page is kept (by the PDF's hash and the size).
	sum := sha256.Sum256(asset.Data)
	key := fmt.Sprintf("%x|%d", sum, maxPx)
	if png, ok := s.pdfCache.rendered.get(key); ok {
		return AssetPreview{ContentType: "image/png", Data: png}, nil
	}
	png, err := s.pdfTools.Render(ctx, asset.Data, maxPx)
	if err != nil {
		s.countPDFToolsError(pdfOpRender, err)
		return AssetPreview{}, err
	}
	if len(png) > maxPDFToolsPNGBytes || len(png) < len(pngSignature) || string(png[:len(pngSignature)]) != string(pngSignature) {
		s.pdfToolsErrors.add(pdfOpRender)
		return AssetPreview{}, pdfToolsUnavailable(errors.New("pdf-tools: render answered no PNG"))
	}
	s.pdfCache.rendered.put(key, png)
	return AssetPreview{ContentType: "image/png", Data: png}, nil
}

// allowedAsset reads a certificate asset (or a legacy Media, which the
// editor also takes) for p: who uploaded it, or who may edit a template
// that links it. Another purpose's Media, and one p may not read, is not
// found: as a Media link refuses one that is not the caller's, it does not
// say the Media exists.
func (s *service) allowedAsset(ctx context.Context, p authz.Principal, id uuid.UUID) (Asset, error) {
	origins, ok := s.assets.(assetOrigins)
	if !ok {
		return Asset{}, invalid(CodeAssetStorageUnavailable)
	}
	origin, err := origins.AssetOrigin(ctx, id)
	if err != nil {
		return Asset{}, err
	}
	if origin.Purpose != media.PurposeCertificateAsset && origin.Purpose != media.PurposeLegacy {
		return Asset{}, ErrNotFound
	}
	allowed, err := s.mayEditAsset(ctx, p, id, origin)
	if err != nil {
		return Asset{}, err
	}
	if !allowed {
		return Asset{}, ErrNotFound
	}
	asset, err := s.decrypted(s.assets).ReadAsset(ctx, id)
	if errors.Is(err, media.ErrNotFound) {
		return Asset{}, ErrNotFound
	}
	if err != nil {
		return Asset{}, err
	}
	if len(asset.Data) == 0 {
		return Asset{}, invalid(CodeAssetEmpty)
	}
	return asset, nil
}

func (s *service) mayEditAsset(ctx context.Context, p authz.Principal, id uuid.UUID, origin AssetOrigin) (bool, error) {
	if caller := principalUUID(p); caller != uuid.Nil && caller == origin.UploadedBy {
		return true, nil
	}
	using, ok := s.templates.(templatesUsingMedia)
	if !ok {
		return false, nil
	}
	templates, err := using.TemplatesUsingMedia(ctx, id)
	if err != nil {
		return false, err
	}
	for _, item := range templates {
		switch err := s.allowTemplate(ctx, p, item, authz.Update); {
		case err == nil:
			return true, nil
		case errors.Is(err, ErrForbidden), errors.Is(err, ErrNotFound):
		default:
			return false, err
		}
	}
	return false, nil
}

// mediaType is a content type without its parameters, in lower case.
func mediaType(contentType string) string {
	return strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
}
