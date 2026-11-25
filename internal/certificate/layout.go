package certificate

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"html"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/organizer"
	"github.com/skylab-kulubu/core-backend/internal/qr"
)

const maxLayoutElements = 100

var cssColorPattern = regexp.MustCompile(`^(#[0-9a-fA-F]{3,8}|[a-zA-Z]{3,20}|rgba?\([0-9., %]+\)|hsla?\([0-9., %]+\))$`)

func ValidateLayout(layout Layout) error {
	if layout.Width < 300 || layout.Width > 4000 || layout.Height < 300 || layout.Height > 4000 {
		return invalid(CodeLayoutSizeInvalid)
	}
	if layout.Orientation != "landscape" && layout.Orientation != "portrait" {
		return invalid(CodeLayoutOrientation)
	}
	if layout.BackgroundColor != "" && !cssColorPattern.MatchString(layout.BackgroundColor) {
		return invalid(CodeLayoutColorInvalid)
	}
	if p := layout.BackgroundPlacement; p != nil && (layout.BackgroundMediaID == nil || !validPlacement(*p)) {
		return invalid(CodeLayoutPlacementInvalid)
	}
	// The original of a converted background stands beside the background
	// it was converted into, never alone.
	if layout.BackgroundOriginalMediaID != nil && layout.BackgroundMediaID == nil {
		return ErrInvalid
	}
	if len(layout.Elements) == 0 || len(layout.Elements) > maxLayoutElements {
		return invalid(CodeLayoutElementCount)
	}
	// A layout must name its recipient. The Event's name is optional
	// (ADR-0040 addendum 2026-11-25): a background may print it already. The
	// verification QR is optional too (addendum 2026-11-24): the default
	// template keeps one, a template may drop it, and the certificate is
	// still verified at its address by serial
	// (yildizskylab.com/sertifika/{serial}). It must keep the QR or the
	// serial, so a paper certificate can be verified.
	hasRecipient, verifiable := false, false
	ids := make(map[string]struct{}, len(layout.Elements))
	for _, el := range layout.Elements {
		if strings.TrimSpace(el.ID) == "" || len(el.ID) > 80 {
			return invalid(CodeLayoutElementID)
		}
		if _, exists := ids[el.ID]; exists {
			return invalid(CodeLayoutElementID)
		}
		ids[el.ID] = struct{}{}
		switch el.Kind {
		case "staticText", "recipientName", "eventName", "eventDates", "issueDate", "ownerTeam", "serial", "verificationQr", "image", "shape":
		default:
			return invalid(CodeLayoutElementKind)
		}
		hasRecipient = hasRecipient || el.Kind == "recipientName"
		verifiable = verifiable || el.Kind == "verificationQr" || el.Kind == "serial"
		if !finite(el.X, el.Y, el.Width, el.Height) ||
			el.X < 0 || el.Y < 0 || el.Width <= 0 || el.Height <= 0 ||
			el.X+el.Width > layout.Width+0.01 || el.Y+el.Height > layout.Height+0.01 {
			return invalid(CodeLayoutOutOfBounds)
		}
		if !finite(el.FontSize, el.MinFontSize, el.LineHeight, el.LetterSpacing, el.BorderWidth, el.BorderRadius, el.Rotation, el.opacity()) ||
			el.FontSize < 0 || el.FontSize > 300 || el.MinFontSize < 0 || el.MinFontSize > 300 ||
			el.FontWeight < 0 || el.FontWeight > 1000 || el.LineHeight < 0 || el.LineHeight > 5 ||
			el.LetterSpacing < -20 || el.LetterSpacing > 100 || el.BorderWidth < 0 || el.BorderWidth > 100 ||
			el.BorderRadius < 0 || el.BorderRadius > 2000 ||
			el.Rotation < -360 || el.Rotation > 360 || el.opacity() < 0 || el.opacity() > 1 {
			return invalid(CodeLayoutElementStyle)
		}
		for _, color := range []string{el.Color, el.BackgroundColor, el.BorderColor} {
			if color != "" && !cssColorPattern.MatchString(color) {
				return invalid(CodeLayoutColorInvalid)
			}
		}
		if el.Align != "" && el.Align != "left" && el.Align != "center" && el.Align != "right" {
			return invalid(CodeLayoutElementStyle)
		}
		if el.Kind == "image" && el.MediaID == nil {
			return invalid(CodeLayoutImageMissing)
		}
		if len(el.Text) > 4000 {
			return invalid(CodeLayoutTextTooLong)
		}
		if len(el.FontFamily) > 80 {
			return invalid(CodeLayoutElementStyle)
		}
		if (el.FontStyle != "" && el.FontStyle != FontStyleItalic) ||
			(el.Origin != "" && el.Origin != OriginBackgroundText) ||
			!utf8.ValidString(el.SourceFont) || utf8.RuneCountInString(el.SourceFont) > maxSourceFontRunes {
			return invalid(CodeLayoutElementStyle)
		}
		// A certificate font sets text: an element that draws none names
		// none.
		if el.FontMediaID != nil && (*el.FontMediaID == uuid.Nil || !drawsText(el.Kind)) {
			return invalid(CodeLayoutElementStyle)
		}
	}
	if !hasRecipient {
		return invalid(CodeLayoutRecipientMissing)
	}
	if !verifiable {
		return invalid(CodeLayoutNotVerifiable)
	}
	return nil
}

func finite(values ...float64) bool {
	for _, value := range values {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return false
		}
	}
	return true
}

func LayoutChecksum(layout Layout) (string, error) {
	raw, err := marshalLayout(layout)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func LayoutHTML(ctx context.Context, layout Layout, data PreviewData, verifyURL string, assets AssetReader) (string, error) {
	if err := ValidateLayout(layout); err != nil {
		return "", err
	}
	background := ""
	var body strings.Builder
	if layout.BackgroundMediaID != nil {
		if layout.BackgroundPlacement == nil {
			uri, err := assetDataURI(ctx, assets, *layout.BackgroundMediaID)
			if err != nil {
				return "", err
			}
			background = `background-image:url("` + uri + `");background-size:cover;background-position:center;`
		} else {
			img, err := placedBackground(ctx, layout, assets)
			if err != nil {
				return "", err
			}
			body.WriteString(img)
		}
	}
	for _, el := range layout.Elements {
		content, image, err := elementContent(ctx, el, data, verifyURL, assets)
		if err != nil {
			return "", err
		}
		style := elementStyle(el)
		if image {
			body.WriteString(`<img alt="" style="` + style + `object-fit:` + imageFit(el.Fit) + `" src="` + content + `"/>`)
			continue
		}
		if el.Kind == "shape" {
			body.WriteString(`<div aria-hidden="true" style="` + style + `"></div>`)
			continue
		}
		fitClass := ""
		fitData := ""
		if el.Fit == "shrink" {
			fitClass = ` class="fit"`
			// .fit is a flex box, where text-align no longer places a
			// short line: map the alignment to justify-content. A fitted
			// box is one line that shrinks (the editor draws it so), so
			// nowrap must beat the element's own pre-wrap, which as an
			// inline style would win over the .fit rule.
			style += "justify-content:" + fitJustify(el.Align) + ";white-space:nowrap;"
			fitData = ` data-min-font="` + number(max(el.MinFontSize, 8)) + `"`
		}
		if el.Kind == "ownerTeam" {
			// "KOD · Türkçe ad" may not fit a box drawn for the code: the
			// page then prints the code alone (the script below).
			if code := strings.TrimSpace(data.OwnerTeam); code != "" && content != code && !organizer.Club(code) {
				// data-font: the designed size, restored before the code
				// is fitted (the shrink pass may have lowered it).
				fitData += ` data-short="` + html.EscapeString(code) + `" data-font="` + number(el.FontSize) + `"`
			}
		}
		body.WriteString(`<div` + fitClass + fitData + ` style="` + style + `">` + html.EscapeString(content) + `</div>`)
	}
	bg := layout.BackgroundColor
	if bg == "" {
		bg = "#ffffff"
	}
	faces, err := fontFaces(ctx, layout, assets)
	if err != nil {
		return "", err
	}
	fitText := `document.querySelectorAll('.fit').forEach(fit)
document.querySelectorAll('[data-short]').forEach(function(el){if(over(el)){el.style.fontSize=el.dataset.font+'px';el.textContent=el.dataset.short;if(el.classList.contains('fit'))fit(el)}})`
	if faces != "" {
		// Measure the text once the embedded fonts are in, not in the
		// fallback the page starts with, then tell the renderer the page
		// is ready to print (fitDoneFlag; also when fitting throws, so
		// the renderer never waits in vain).
		fitText = `document.fonts.ready.then(function(){try{
` + fitText + `
}finally{` + fitDoneFlag + `}})`
	}
	return `<!doctype html><html lang="tr"><head><meta charset="utf-8"><style>
` + faces + `@page{size:` + number(layout.Width) + `px ` + number(layout.Height) + `px;margin:0}*{box-sizing:border-box}html,body{margin:0;padding:0;width:100%;height:100%;overflow:hidden}body{font-family:Arial,sans-serif}.page{position:relative;width:` + number(layout.Width) + `px;height:` + number(layout.Height) + `px;overflow:hidden;background:` + html.EscapeString(bg) + `;` + background + `}.fit{display:flex;align-items:center;white-space:nowrap;overflow:hidden}
</style></head><body><main class="page">` + body.String() + `</main><script>
function over(el){return el.scrollWidth>el.clientWidth||el.scrollHeight>el.clientHeight}function fit(el){var size=parseFloat(getComputedStyle(el).fontSize);var min=parseFloat(el.dataset.minFont||'8');while(over(el)&&size>min){size-=1;el.style.fontSize=size+'px'}}
` + fitText + `
</script></body></html>`, nil
}

func elementContent(ctx context.Context, el Element, data PreviewData, verifyURL string, assets AssetReader) (string, bool, error) {
	switch el.Kind {
	case "staticText":
		return el.Text, false, nil
	case "recipientName":
		return data.RecipientName, false, nil
	case "eventName":
		return data.EventName, false, nil
	case "eventDates":
		return data.EventDates, false, nil
	case "issueDate":
		return data.IssueDate, false, nil
	case "ownerTeam":
		// The organizer as the club shows it (organizer.Name): the club's
		// name for YK, DK, ADMIN or no Owner team.
		return organizer.Name(data.OwnerTeam), false, nil
	case "serial":
		return data.Serial, false, nil
	case "verificationQr":
		png, err := qr.PNG(verifyURL, int(math.Max(el.Width, el.Height)*2))
		if err != nil {
			return "", false, err
		}
		return "data:image/png;base64," + base64.StdEncoding.EncodeToString(png), true, nil
	case "image":
		if el.MediaID == nil {
			return "", false, invalid(CodeLayoutImageMissing)
		}
		uri, err := assetDataURI(ctx, assets, *el.MediaID)
		return uri, true, err
	case "shape":
		return "", false, nil
	default:
		return "", false, invalid(CodeLayoutElementKind)
	}
}

func assetDataURI(ctx context.Context, assets AssetReader, id uuid.UUID) (string, error) {
	_, uri, err := imageAsset(ctx, assets, id)
	return uri, err
}

// imageAsset reads an image Media and its data URI.
func imageAsset(ctx context.Context, assets AssetReader, id uuid.UUID) (Asset, string, error) {
	if assets == nil {
		return Asset{}, "", invalid(CodeAssetStorageUnavailable)
	}
	asset, err := assets.ReadAsset(ctx, id)
	if err != nil {
		return Asset{}, "", err
	}
	switch {
	case !strings.HasPrefix(asset.ContentType, "image/"):
		return Asset{}, "", invalid(CodeAssetTypeUnsupported)
	case len(asset.Data) == 0:
		return Asset{}, "", invalid(CodeAssetEmpty)
	case len(asset.Data) > 20<<20:
		return Asset{}, "", invalid(CodeAssetTooLarge)
	}
	return asset, "data:" + asset.ContentType + ";base64," + base64.StdEncoding.EncodeToString(asset.Data), nil
}

// placedBackground is the image background of a layout with a placement:
// an <img> in the box PlaceBackground gives, under every element. The
// page clips what reaches past it.
func placedBackground(ctx context.Context, layout Layout, assets AssetReader) (string, error) {
	asset, uri, err := imageAsset(ctx, assets, *layout.BackgroundMediaID)
	if err != nil {
		return "", err
	}
	w, h, err := backgroundSize(asset.ContentType, asset.Data)
	if err != nil {
		return "", err
	}
	box := PlaceBackground(layout.Width, layout.Height, w, h, *layout.BackgroundPlacement)
	return `<img alt="" aria-hidden="true" style="position:absolute;left:` + number(box.Left) + `px;top:` + number(box.Top) + `px;width:` + number(box.Width) + `px;height:` + number(box.Height) + `px" src="` + uri + `"/>`, nil
}

func elementStyle(el Element) string {
	align := el.Align
	if align == "" {
		align = "left"
	}
	color := el.Color
	if color == "" {
		color = "#111111"
	}
	font := safeFont(el.FontFamily)
	if el.FontMediaID != nil {
		// The certificate font first (fontFaces embeds it), then the
		// family's stack: a letter the font lacks falls back to it.
		font = `'` + uploadedFontFamily(*el.FontMediaID) + `',` + font
	}
	lineHeight := el.LineHeight
	if lineHeight == 0 {
		lineHeight = 1.15
	}
	background := el.BackgroundColor
	if background == "" {
		background = "transparent"
	}
	borderColor := el.BorderColor
	if borderColor == "" {
		borderColor = "transparent"
	}
	style := fmt.Sprintf("position:absolute;left:%spx;top:%spx;width:%spx;height:%spx;font-family:%s;font-size:%spx;font-weight:%d;color:%s;background:%s;border:%spx solid %s;border-radius:%spx;text-align:%s;line-height:%s;letter-spacing:%spx;opacity:%s;transform:rotate(%sdeg);transform-origin:center;white-space:pre-wrap;overflow:hidden;", number(el.X), number(el.Y), number(el.Width), number(el.Height), font, number(el.FontSize), el.FontWeight, color, background, number(el.BorderWidth), borderColor, number(el.BorderRadius), align, number(lineHeight), number(el.LetterSpacing), number(el.opacity()), number(el.Rotation))
	// Only an italic element says so: every other element's style, and
	// so the page of a layout written before fontStyle, is as before.
	if el.FontStyle == FontStyleItalic {
		style += "font-style:italic;"
	}
	return style
}

// drawsText reports whether an element of the kind is text.
func drawsText(kind string) bool {
	switch kind {
	case "image", "shape", "verificationQr":
		return false
	}
	return true
}

// opacity is the element's opacity; a layout that leaves it out means 1.
func (el Element) opacity() float64 {
	if el.Opacity == nil {
		return 1
	}
	return *el.Opacity
}

func safeFont(font string) string {
	if catalog, ok := catalogFontNamed(font); ok {
		return catalog.Stack
	}
	switch strings.ToLower(strings.TrimSpace(font)) {
	case "arial":
		return `'Liberation Sans',Arial,sans-serif`
	case "helvetica":
		return `'Liberation Sans',Helvetica,sans-serif`
	case "carlito":
		return `Carlito,'Liberation Sans',sans-serif`
	case "noto sans":
		return `'Noto Sans','DejaVu Sans',sans-serif`
	case "noto sans display":
		return `'Noto Sans Display','Noto Sans',sans-serif`
	case "dejavu sans":
		return `'DejaVu Sans','Liberation Sans',sans-serif`
	case "dejavu sans condensed":
		return `'DejaVu Sans Condensed','DejaVu Sans',sans-serif`
	case "liberation sans":
		return `'Liberation Sans',Arial,sans-serif`
	case "georgia":
		return `'Liberation Serif',Georgia,serif`
	case "times new roman":
		return `'Liberation Serif','Times New Roman',serif`
	case "caladea":
		return `Caladea,'Liberation Serif',serif`
	case "noto serif":
		return `'Noto Serif','DejaVu Serif',serif`
	case "noto serif display":
		return `'Noto Serif Display','Noto Serif',serif`
	case "dejavu serif":
		return `'DejaVu Serif','Liberation Serif',serif`
	case "dejavu serif condensed":
		return `'DejaVu Serif Condensed','DejaVu Serif',serif`
	case "liberation serif":
		return `'Liberation Serif','Times New Roman',serif`
	case "courier new":
		return `'Liberation Mono','Courier New',monospace`
	case "dejavu sans mono":
		return `'DejaVu Sans Mono','Liberation Mono',monospace`
	case "liberation mono":
		return `'Liberation Mono','Courier New',monospace`
	default:
		return `'Liberation Sans',Arial,sans-serif`
	}
}

// fitJustify maps an element's text alignment to the flex
// justify-content of a shrink-to-fit box. "safe" sends a line that is
// still too wide to the right edge only: overflow past the left edge is
// not counted in scrollWidth, so the fit script would never shrink a
// right-aligned name. Once the line fits, safe and plain align alike.
func fitJustify(align string) string {
	switch align {
	case "center":
		return "safe center"
	case "right":
		return "safe flex-end"
	default:
		return "flex-start"
	}
}

func imageFit(fit string) string {
	if fit == "contain" {
		return "contain"
	}
	return "cover"
}

func number(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}
