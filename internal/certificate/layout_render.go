package certificate

import (
	"bytes"
	"context"
	"strings"
)

func (s *service) renderLayoutPDF(ctx context.Context, layout Layout, data PreviewData, verifyURL string, assets AssetReader) ([]byte, error) {
	if s.render == nil {
		return nil, rendererUnavailable(nil)
	}
	prepared := layout
	var backgroundPDF []byte
	if layout.BackgroundMediaID != nil {
		if assets == nil {
			return nil, invalid(CodeAssetStorageUnavailable)
		}
		asset, err := assets.ReadAsset(ctx, *layout.BackgroundMediaID)
		if err != nil {
			return nil, err
		}
		if strings.EqualFold(strings.TrimSpace(asset.ContentType), "application/pdf") {
			switch {
			case len(asset.Data) > 20<<20:
				return nil, invalid(CodeBackgroundPDFTooLarge)
			case !bytes.Contains(asset.Data[:min(len(asset.Data), 1024)], []byte("%PDF-")):
				return nil, invalid(CodeBackgroundPDFUnreadable)
			}
			backgroundPDF = asset.Data
			prepared.BackgroundMediaID = nil
			// A PDF background is printed as before for now, also when the
			// layout places it: the placed copy comes with pdf-tools.
			prepared.BackgroundPlacement = nil
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
		return nil, rendererUnavailable(nil)
	}
	return renderer.PDFWithBackground(ctx, html, backgroundPDF)
}
