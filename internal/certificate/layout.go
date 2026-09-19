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

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/qr"
)

const maxLayoutElements = 100

var cssColorPattern = regexp.MustCompile(`^(#[0-9a-fA-F]{3,8}|[a-zA-Z]{3,20}|rgba?\([0-9., %]+\)|hsla?\([0-9., %]+\))$`)

func ValidateLayout(layout Layout) error {
	if layout.Width < 300 || layout.Width > 4000 || layout.Height < 300 || layout.Height > 4000 {
		return ErrInvalid
	}
	if layout.Orientation != "landscape" && layout.Orientation != "portrait" {
		return ErrInvalid
	}
	if layout.BackgroundColor != "" && !cssColorPattern.MatchString(layout.BackgroundColor) {
		return ErrInvalid
	}
	if len(layout.Elements) == 0 || len(layout.Elements) > maxLayoutElements {
		return ErrInvalid
	}
	required := map[string]bool{"recipientName": false, "eventName": false, "verificationQr": false}
	ids := make(map[string]struct{}, len(layout.Elements))
	for _, el := range layout.Elements {
		if strings.TrimSpace(el.ID) == "" || len(el.ID) > 80 {
			return ErrInvalid
		}
		if _, exists := ids[el.ID]; exists {
			return ErrInvalid
		}
		ids[el.ID] = struct{}{}
		switch el.Kind {
		case "staticText", "recipientName", "eventName", "eventDates", "issueDate", "ownerTeam", "serial", "verificationQr", "image", "shape":
		default:
			return ErrInvalid
		}
		if _, ok := required[el.Kind]; ok {
			required[el.Kind] = true
		}
		if !finite(el.X, el.Y, el.Width, el.Height, el.FontSize, el.MinFontSize, el.LineHeight, el.LetterSpacing, el.BorderWidth, el.BorderRadius, el.Rotation, el.Opacity) ||
			el.X < 0 || el.Y < 0 || el.Width <= 0 || el.Height <= 0 ||
			el.X+el.Width > layout.Width+0.01 || el.Y+el.Height > layout.Height+0.01 ||
			el.FontSize < 0 || el.FontSize > 300 || el.MinFontSize < 0 || el.MinFontSize > 300 ||
			el.FontWeight < 0 || el.FontWeight > 1000 || el.LineHeight < 0 || el.LineHeight > 5 ||
			el.LetterSpacing < -20 || el.LetterSpacing > 100 || el.BorderWidth < 0 || el.BorderWidth > 100 ||
			el.BorderRadius < 0 || el.BorderRadius > 2000 ||
			el.Rotation < -360 || el.Rotation > 360 || el.Opacity < 0 || el.Opacity > 1 {
			return ErrInvalid
		}
		for _, color := range []string{el.Color, el.BackgroundColor, el.BorderColor} {
			if color != "" && !cssColorPattern.MatchString(color) {
				return ErrInvalid
			}
		}
		if el.Align != "" && el.Align != "left" && el.Align != "center" && el.Align != "right" {
			return ErrInvalid
		}
		if el.Kind == "image" && el.MediaID == nil {
			return ErrInvalid
		}
		if len(el.Text) > 4000 {
			return ErrInvalid
		}
		if len(el.FontFamily) > 80 {
			return ErrInvalid
		}
	}
	for _, present := range required {
		if !present {
			return ErrInvalid
		}
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
	if layout.BackgroundMediaID != nil {
		uri, err := assetDataURI(ctx, assets, *layout.BackgroundMediaID)
		if err != nil {
			return "", err
		}
		background = `background-image:url("` + uri + `");background-size:cover;background-position:center;`
	}
	var body strings.Builder
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
			fitData = ` data-min-font="` + number(max(el.MinFontSize, 8)) + `"`
		}
		body.WriteString(`<div` + fitClass + fitData + ` style="` + style + `">` + html.EscapeString(content) + `</div>`)
	}
	bg := layout.BackgroundColor
	if bg == "" {
		bg = "#ffffff"
	}
	return `<!doctype html><html lang="tr"><head><meta charset="utf-8"><style>
@page{size:` + number(layout.Width) + `px ` + number(layout.Height) + `px;margin:0}*{box-sizing:border-box}html,body{margin:0;padding:0;width:100%;height:100%;overflow:hidden}body{font-family:Arial,sans-serif}.page{position:relative;width:` + number(layout.Width) + `px;height:` + number(layout.Height) + `px;overflow:hidden;background:` + html.EscapeString(bg) + `;` + background + `}.fit{display:flex;align-items:center;justify-content:inherit;white-space:nowrap;overflow:hidden}
</style></head><body><main class="page">` + body.String() + `</main><script>
document.querySelectorAll('.fit').forEach(function(el){var size=parseFloat(getComputedStyle(el).fontSize);var min=parseFloat(el.dataset.minFont||'8');while((el.scrollWidth>el.clientWidth||el.scrollHeight>el.clientHeight)&&size>min){size-=1;el.style.fontSize=size+'px'}})
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
		return data.OwnerTeam, false, nil
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
			return "", false, ErrInvalid
		}
		uri, err := assetDataURI(ctx, assets, *el.MediaID)
		return uri, true, err
	case "shape":
		return "", false, nil
	default:
		return "", false, ErrInvalid
	}
}

func assetDataURI(ctx context.Context, assets AssetReader, id uuid.UUID) (string, error) {
	if assets == nil {
		return "", ErrInvalid
	}
	asset, err := assets.ReadAsset(ctx, id)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(asset.ContentType, "image/") || len(asset.Data) == 0 || len(asset.Data) > 20<<20 {
		return "", ErrInvalid
	}
	return "data:" + asset.ContentType + ";base64," + base64.StdEncoding.EncodeToString(asset.Data), nil
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
	return fmt.Sprintf("position:absolute;left:%spx;top:%spx;width:%spx;height:%spx;font-family:%s;font-size:%spx;font-weight:%d;color:%s;background:%s;border:%spx solid %s;border-radius:%spx;text-align:%s;line-height:%s;letter-spacing:%spx;opacity:%s;transform:rotate(%sdeg);transform-origin:center;white-space:pre-wrap;overflow:hidden;", number(el.X), number(el.Y), number(el.Width), number(el.Height), font, number(el.FontSize), el.FontWeight, color, background, number(el.BorderWidth), borderColor, number(el.BorderRadius), align, number(lineHeight), number(el.LetterSpacing), number(el.Opacity), number(el.Rotation))
}

func safeFont(font string) string {
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

func imageFit(fit string) string {
	if fit == "contain" {
		return "contain"
	}
	return "cover"
}

func number(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}
