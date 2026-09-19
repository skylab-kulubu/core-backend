package certificate

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
)

type renderAssetReader struct {
	assets map[uuid.UUID]Asset
}

func (r renderAssetReader) ReadAsset(_ context.Context, id uuid.UUID) (Asset, error) {
	asset, ok := r.assets[id]
	if !ok {
		return Asset{}, ErrNotFound
	}
	return asset, nil
}

type backgroundRecorder struct {
	html       string
	background []byte
}

func (r *backgroundRecorder) PDF(_ context.Context, html string) ([]byte, error) {
	r.html = html
	return []byte("%PDF-plain"), nil
}

func (r *backgroundRecorder) PDFWithBackground(_ context.Context, html string, background []byte) ([]byte, error) {
	r.html = html
	r.background = append([]byte(nil), background...)
	return []byte("%PDF-composed"), nil
}

func TestRenderLayoutPreservesPDFBackgroundForVectorComposition(t *testing.T) {
	t.Parallel()
	id := uuid.New()
	background := []byte("%PDF-1.7 vector certificate")
	assets := renderAssetReader{assets: map[uuid.UUID]Asset{
		id: {ContentType: "application/pdf", Data: background},
	}}
	renderer := &backgroundRecorder{}
	service := &service{render: renderer}
	layout := Layout{
		Width: 1123, Height: 794, Orientation: "landscape", BackgroundColor: "#ffffff", BackgroundMediaID: &id,
		Elements: []Element{
			{ID: "name", Kind: "recipientName", X: 100, Y: 100, Width: 800, Height: 80, FontSize: 40, FontFamily: "Noto Sans Display", Opacity: 1},
			{ID: "event", Kind: "eventName", X: 100, Y: 220, Width: 800, Height: 60, FontSize: 28, Opacity: 1},
			{ID: "qr", Kind: "verificationQr", X: 900, Y: 600, Width: 130, Height: 130, Opacity: 1},
		},
	}

	pdf, err := service.renderLayoutPDF(t.Context(), layout, PreviewData{RecipientName: "Ada", EventName: "SKY"}, "https://skyl.app/c/example", assets)
	if err != nil {
		t.Fatal(err)
	}
	if string(pdf) != "%PDF-composed" || !bytes.Equal(renderer.background, background) {
		t.Fatalf("pdf=%q background=%q", pdf, renderer.background)
	}
	if strings.Contains(renderer.html, "data:application/pdf") || !strings.Contains(renderer.html, "background:transparent") {
		t.Fatalf("overlay HTML must stay transparent: %s", renderer.html)
	}
}
