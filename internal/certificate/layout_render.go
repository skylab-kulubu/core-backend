package certificate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
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
		if isPDF(asset.ContentType) {
			if err := checkBackgroundPDF(asset.Data); err != nil {
				return nil, err
			}
			backgroundPDF = asset.Data
			if layout.BackgroundPlacement != nil {
				if backgroundPDF, err = s.placedBackground(ctx, layout, asset.Data, assets); err != nil {
					return nil, err
				}
			}
			prepared.BackgroundMediaID = nil
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

// placedBackground is the PDF background of a layout with a placement as
// it is laid under the page: a published version's placed copy, read with
// its assets (issuance never calls pdf-tools); for a draft, placed now by
// pdf-tools (placePDF, cached). A version published before placed copies
// prints its PDF as it was published: as before, by Gotenberg's scale
// (LegacyPDFPlacement), its placement not applied.
func (s *service) placedBackground(ctx context.Context, layout Layout, pdf []byte, assets AssetReader) ([]byte, error) {
	if versions, ok := assets.(placedAssetReader); ok {
		placed, found, err := versions.ReadPlaced(ctx, *layout.BackgroundMediaID)
		switch {
		case errors.Is(err, errNotAVersion):
		case err != nil:
			return nil, err
		case !found:
			return pdf, nil
		default:
			sum := sha256.Sum256(placed.Data)
			if hex.EncodeToString(sum[:]) != placed.checksum {
				return nil, fmt.Errorf("certificate: the placed copy of %s does not match its checksum", layout.BackgroundMediaID)
			}
			return placed.Data, nil
		}
	}
	return s.placePDF(ctx, layout, pdf)
}
