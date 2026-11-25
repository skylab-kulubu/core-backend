package certificate

import (
	"bytes"
	"context"
	"errors"
	"path"
	"strings"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

// AssetUploader stores a file core made as a new Media of a purpose, for
// the person asking (media.Service does): the PDF a background text
// conversion leaves.
type AssetUploader interface {
	UploadForPurpose(ctx context.Context, p authz.Principal, purpose string, file media.UploadedFile) (media.Media, error)
}

// AssetText is a PDF asset's text as the editor lists it (GET
// /v1/certificate-assets/{id}/text): one piece per run (one style along a
// line), in reading order, with the font each would be set in.
type AssetText struct {
	Spans []AssetTextSpan `json:"spans"`
	// Truncated: the PDF has more text than pdf-tools reads (5000 spans);
	// none of it can be converted.
	Truncated bool `json:"truncated"`
	// TextMissing says why a PDF lists no text at all: "outlines" (it
	// draws shapes: its text was turned into outlines, as Canva's and
	// Illustrator's outline export does), "image" (the page is a
	// picture), "unknown". Empty when it has text.
	TextMissing string `json:"textMissing,omitempty"`
}

// AssetTextSpan is one piece of a PDF's text.
type AssetTextSpan struct {
	// ID names the piece in a conversion (TextConversionRequest.SpanIDs).
	ID   string `json:"id"`
	Text string `json:"text"`
	// SourceFont is the PDF's font name (without a subset prefix); Font,
	// FontWeight and FontStyle what a conversion sets it in, FontSubstituted
	// whether Font stands in for another family.
	SourceFont      string `json:"sourceFont"`
	Font            string `json:"font"`
	FontWeight      int    `json:"fontWeight"`
	FontStyle       string `json:"fontStyle,omitempty"`
	FontSubstituted bool   `json:"fontSubstituted"`
	// FontSizePt is the size the PDF draws it at, pt; the element's px
	// size follows the background's placement.
	FontSizePt  float64 `json:"fontSizePt"`
	Convertible bool    `json:"convertible"`
	// Reason is why it cannot be converted (pdf-tools' reasons, below the
	// README's table, and "limit": past the first 100 convertible pieces,
	// "truncated": the PDF has more text than is read).
	Reason string `json:"reason,omitempty"`
}

// TextConversionRequest converts chosen pieces of a PDF background's text
// (POST /v1/certificate-assets/{id}/text-conversion).
type TextConversionRequest struct {
	SpanIDs []string `json:"spanIds"`
	// Kinds turns a chosen piece into recipientName, eventName,
	// eventDates or issueDate (its box and style kept, its text not).
	Kinds map[string]string `json:"kinds"`
	// Page is the layout's page in px; Placement places the background on
	// it (absent: as a layout without one prints a PDF).
	Page struct {
		Width  float64 `json:"width"`
		Height float64 `json:"height"`
	} `json:"page"`
	Placement *BackgroundPlacement `json:"placement"`
	// ExistingElements is how many elements the draft keeps beside the new
	// ones; together they stay within 100.
	ExistingElements int `json:"existingElements"`
}

// TextConversion is a conversion's answer, which the editor applies to its
// draft; nothing is saved until the draft is.
type TextConversion struct {
	// BackgroundMediaID is the new background: the PDF without the
	// converted text, a pending certificate asset of the caller's that
	// expires in 24 hours unless a template links it.
	BackgroundMediaID uuid.UUID `json:"backgroundMediaId"`
	// BackgroundOriginalMediaID is the PDF converted (the request's).
	BackgroundOriginalMediaID uuid.UUID     `json:"backgroundOriginalMediaId"`
	Elements                  []Element     `json:"elements"`
	Warnings                  []TextWarning `json:"warnings"`
}

// TextWarning is something a conversion could not keep as the PDF had it.
type TextWarning struct {
	SpanID    string `json:"spanId"`
	ElementID string `json:"elementId"`
	Kind      string `json:"kind"`
	From      string `json:"from,omitempty"`
	To        string `json:"to,omitempty"`
}

// The kinds of TextWarning.
const (
	// WarningFontSubstituted: the PDF's font (From) is set in another
	// family (To).
	WarningFontSubstituted = "fontSubstituted"
	// WarningFontSizeBounded: a size over 300 px (From) is set at 300.
	WarningFontSizeBounded = "fontSizeBounded"
)

// The bounds of a conversion request.
const (
	maxConversionSpanIDs = 500
	// outlinedPathCount: a page with no text drawing at least this many
	// shapes had its text turned into outlines.
	outlinedPathCount = 20
	textReasonLimit   = "limit"
	textReasonCut     = "truncated"
)

func (s *service) AssetText(ctx context.Context, p authz.Principal, id uuid.UUID) (AssetText, error) {
	_, text, err := s.assetPDFText(ctx, p, id)
	if err != nil {
		return AssetText{}, err
	}
	spans := make(map[string]PDFSpan, len(text.Spans))
	for _, span := range text.Spans {
		spans[span.ID] = span
	}
	out := AssetText{Spans: make([]AssetTextSpan, 0, len(text.Runs)), Truncated: text.Truncated}
	convertible := 0
	for _, run := range text.Runs {
		first := spans[run.ID()]
		font := mapFont(first.Font, nil)
		piece := AssetTextSpan{
			ID: run.ID(), Text: strings.TrimSpace(run.Text), SourceFont: stripSubset(first.Font.Name),
			Font: font.Family, FontWeight: font.Weight, FontStyle: font.Style, FontSubstituted: font.Substituted,
			FontSizePt: round2(first.Size), Convertible: true,
		}
		if reason := runReason(run, spans); reason != "" {
			piece.Convertible, piece.Reason = false, reason
		}
		switch {
		case !piece.Convertible:
		case text.Truncated:
			piece.Convertible, piece.Reason = false, textReasonCut
		case convertible >= maxLayoutElements:
			piece.Convertible, piece.Reason = false, textReasonLimit
		default:
			convertible++
		}
		out.Spans = append(out.Spans, piece)
	}
	if len(out.Spans) == 0 {
		out.TextMissing = textMissing(text.Objects)
	}
	return out, nil
}

func (s *service) ConvertAssetText(ctx context.Context, p authz.Principal, id uuid.UUID, req TextConversionRequest) (TextConversion, error) {
	if err := checkConversionRequest(req); err != nil {
		return TextConversion{}, err
	}
	if s.uploads == nil {
		return TextConversion{}, invalid(CodeAssetStorageUnavailable)
	}
	pdf, text, err := s.assetPDFText(ctx, p, id)
	if err != nil {
		return TextConversion{}, err
	}
	if text.Info.Pages != 1 {
		return TextConversion{}, invalid(CodeBackgroundPDFPages)
	}
	if len(text.Runs) == 0 {
		return TextConversion{}, invalid(CodeBackgroundPDFNoText)
	}
	runs := make(map[string]PDFRun, len(text.Runs))
	spans := make(map[string]PDFSpan, len(text.Spans))
	for _, span := range text.Spans {
		spans[span.ID] = span
	}
	for _, run := range text.Runs {
		runs[run.ID()] = run
	}
	var spanIDs []string
	for _, chosen := range req.SpanIDs {
		run, ok := runs[chosen]
		if !ok || text.Truncated || runReason(run, spans) != "" {
			return TextConversion{}, invalid(CodeBackgroundTextSelectionEmpty)
		}
		spanIDs = append(spanIDs, run.SpanIDs...)
	}
	placement := LegacyPDFPlacement(text.Info.WidthPt, text.Info.HeightPt)
	if req.Placement != nil {
		placement = *req.Placement
	}
	box := PlaceBackground(req.Page.Width, req.Page.Height, text.Info.WidthPt*pxPerPt, text.Info.HeightPt*pxPerPt, placement)
	elements, warnings := textElements(text, req.SpanIDs, req.Kinds, req.Page.Width, req.Page.Height, box, nil, func() string {
		return "bgtext-" + uuid.NewString()
	})
	if len(elements) == 0 {
		// Every chosen piece lies outside the page as placed.
		return TextConversion{}, invalid(CodeBackgroundTextSelectionEmpty)
	}
	if len(elements)+req.ExistingElements > maxLayoutElements {
		return TextConversion{}, invalid(CodeLayoutElementCount)
	}
	stripped, err := s.pdfTools.StripText(ctx, pdf, spanIDs)
	if err != nil {
		s.countPDFToolsError(pdfOpStrip, err)
		return TextConversion{}, err
	}
	if len(stripped) > maxBackgroundPDFBytes || !bytes.HasPrefix(stripped, []byte("%PDF-")) {
		s.pdfToolsErrors.add(pdfOpStrip)
		return TextConversion{}, pdfToolsUnavailable(errors.New("pdf-tools: strip-text answered no PDF"))
	}
	name := "arka-plan"
	if origins, ok := s.assets.(assetOrigins); ok {
		if origin, err := origins.AssetOrigin(ctx, id); err == nil && origin.Name != "" {
			name = strings.TrimSuffix(origin.Name, path.Ext(origin.Name))
		}
	}
	created, err := s.uploads.UploadForPurpose(ctx, p, media.PurposeCertificateAsset, media.UploadedFile{
		Name: name + "-metinsiz.pdf", ContentType: "application/pdf", Data: stripped,
	})
	if err != nil {
		return TextConversion{}, uploadRefusal(err)
	}
	if warnings == nil {
		warnings = []TextWarning{}
	}
	return TextConversion{BackgroundMediaID: created.ID, BackgroundOriginalMediaID: id, Elements: elements, Warnings: warnings}, nil
}

// assetPDFText reads a PDF asset p may edit and its text.
func (s *service) assetPDFText(ctx context.Context, p authz.Principal, id uuid.UUID) ([]byte, PDFText, error) {
	asset, err := s.allowedAsset(ctx, p, id)
	if err != nil {
		return nil, PDFText{}, err
	}
	if !isPDF(asset.ContentType) {
		return nil, PDFText{}, invalid(CodeAssetTypeUnsupported)
	}
	if s.pdfTools == nil {
		return nil, PDFText{}, pdfToolsUnavailable(nil)
	}
	if err := checkBackgroundPDF(asset.Data); err != nil {
		return nil, PDFText{}, err
	}
	text, err := s.pdfTools.Text(ctx, asset.Data)
	if err != nil {
		s.countPDFToolsError(pdfOpInspect, err)
		return nil, PDFText{}, err
	}
	if text.Info.Encrypted {
		return nil, PDFText{}, invalid(CodeBackgroundPDFEncrypted)
	}
	return asset.Data, text, nil
}

// runReason is why a run cannot be converted: its first span's that cannot.
func runReason(run PDFRun, spans map[string]PDFSpan) string {
	for _, id := range run.SpanIDs {
		if span := spans[id]; !span.Convertible {
			if span.Reason == "" {
				return "unmatched"
			}
			return span.Reason
		}
	}
	return ""
}

// textMissing says why a page has no text, from what it draws.
func textMissing(objects *PDFObjects) string {
	switch {
	case objects == nil:
		return "unknown"
	case objects.Path >= outlinedPathCount:
		return "outlines"
	case objects.Image > 0:
		return "image"
	default:
		return "unknown"
	}
}

func checkConversionRequest(req TextConversionRequest) error {
	if len(req.SpanIDs) == 0 {
		return invalid(CodeBackgroundTextSelectionEmpty)
	}
	if len(req.SpanIDs) > maxConversionSpanIDs || req.ExistingElements < 0 || req.ExistingElements > maxLayoutElements {
		return ErrInvalid
	}
	if !finite(req.Page.Width, req.Page.Height) || req.Page.Width < 300 || req.Page.Width > 4000 || req.Page.Height < 300 || req.Page.Height > 4000 {
		return invalid(CodeLayoutSizeInvalid)
	}
	if req.Placement != nil && !validPlacement(*req.Placement) {
		return invalid(CodeLayoutPlacementInvalid)
	}
	chosen := make(map[string]bool, len(req.SpanIDs))
	for _, id := range req.SpanIDs {
		if !spanIDPattern.MatchString(id) || chosen[id] {
			return ErrInvalid
		}
		chosen[id] = true
	}
	for id, kind := range req.Kinds {
		if !chosen[id] || !convertibleKinds[kind] {
			return ErrInvalid
		}
	}
	return nil
}

// uploadRefusal is the new background's upload refused, in the certificate
// API's words.
func uploadRefusal(err error) error {
	var refusal *media.PurposeRefusal
	if errors.As(err, &refusal) {
		switch {
		case errors.Is(refusal.Err, media.ErrPurposeForbidden):
			return ErrForbidden
		case errors.Is(refusal.Err, media.ErrPrivateMediaDisabled):
			return invalid(CodeAssetStorageUnavailable)
		case errors.Is(refusal.Err, media.ErrTooLarge):
			return invalid(CodeBackgroundPDFTooLarge)
		}
		return invalid(CodeAssetStorageUnavailable)
	}
	if errors.Is(err, media.ErrPrivateMediaDisabled) {
		return invalid(CodeAssetStorageUnavailable)
	}
	return err
}
