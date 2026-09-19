package certificate

import (
	"bytes"
	"context"
	"strings"
)

func (s *service) renderLayoutPDF(ctx context.Context, layout Layout, data PreviewData, verifyURL string, assets AssetReader) ([]byte, error) {
	if s.render == nil {
		return nil, ErrInvalid
	}
	prepared := layout
	var backgroundPDF []byte
	if layout.BackgroundMediaID != nil {
		if assets == nil {
			return nil, ErrInvalid
		}
		asset, err := assets.ReadAsset(ctx, *layout.BackgroundMediaID)
		if err != nil {
			return nil, err
		}
		if strings.EqualFold(strings.TrimSpace(asset.ContentType), "application/pdf") {
			if len(asset.Data) == 0 || len(asset.Data) > 20<<20 || !bytes.Contains(asset.Data[:min(len(asset.Data), 1024)], []byte("%PDF-")) {
				return nil, ErrInvalid
			}
			backgroundPDF = asset.Data
			prepared.BackgroundMediaID = nil
			prepared.BackgroundColor = "transparent"
		}
	}
	html, err := LayoutHTML(ctx, prepared, data, verifyURL, assets)
	if err != nil {
		return nil, err
	}
	if len(backgroundPDF) == 0 {
		return s.render.PDF(ctx, html)
	}
	renderer, ok := s.render.(PDFBackgroundRenderer)
	if !ok {
		return nil, ErrInvalid
	}
	return renderer.PDFWithBackground(ctx, html, backgroundPDF)
}
