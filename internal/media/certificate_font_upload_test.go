package media_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

// A certificate font is told by its first bytes and kept as it came,
// private: TrueType, OpenType (CFF) and WOFF2. A collection, WOFF 1 and
// anything else are refused, as is a font over 10 MiB; who may not make a
// certificate template may not upload one (docs/certificate-fonts.md).
func TestService_CertificateFontIsToldByItsFirstBytes(t *testing.T) {
	t.Parallel()
	pm := newPrivateMedia(t)
	ctx := context.Background()
	leader := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
	for _, c := range []struct {
		name, want string
		data       []byte
	}{
		{"a.ttf", "font/ttf", []byte("\x00\x01\x00\x00 truetype")},
		{"a.ttf", "font/ttf", []byte("true mac truetype")},
		{"a.otf", "font/otf", []byte("OTTO cff")},
		{"a.woff2", "font/woff2", []byte("wOF2\x00\x01\x00\x00 woff2")},
	} {
		created, err := pm.svc.UploadForPurpose(ctx, leader, media.PurposeCertificateFont, uploaded(c.name, "application/octet-stream", c.data))
		if err != nil {
			t.Fatalf("%s: %v", c.want, err)
		}
		if created.Type != c.want || created.Kind != media.KindFile || created.Visibility != media.VisibilityPrivate || created.URL != "" {
			t.Fatalf("%s: created %+v", c.want, created)
		}
		object, ok := pm.private.Get(created.Key)
		if !ok || bytes.Contains(object, c.data) {
			t.Fatalf("%s: not stored encrypted", c.want)
		}
	}
	for name, data := range map[string][]byte{
		"collection": []byte("ttcf collection"),
		"WOFF 1":     []byte("wOFF woff1"),
		"PDF":        []byte("%PDF-1.7\n"),
		"text":       []byte("hello"),
	} {
		_, err := pm.svc.UploadForPurpose(ctx, leader, media.PurposeCertificateFont, uploaded("a.ttf", "font/ttf", data))
		if !errors.Is(err, media.ErrTypeNotAllowed) {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
	big := append([]byte("\x00\x01\x00\x00"), make([]byte, 10<<20)...)
	if _, err := pm.svc.UploadForPurpose(ctx, leader, media.PurposeCertificateFont, uploaded("big.ttf", "font/ttf", big)); !errors.Is(err, media.ErrTooLarge) {
		t.Fatalf("over 10 MiB: err = %v", err)
	}
	// A GECEKODU member edits its Events (and uploads their certificate
	// backgrounds) but makes no template: no font.
	member := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ORGANIZASYON/GECEKODU"}}
	if _, err := pm.svc.UploadForPurpose(ctx, member, media.PurposeCertificateFont, uploaded("a.ttf", "font/ttf", []byte("OTTO"))); !errors.Is(err, media.ErrPurposeForbidden) {
		t.Fatalf("Event editor: err = %v", err)
	}
	// An image purpose does not take a font.
	if _, err := pm.svc.UploadForPurpose(ctx, leader, media.PurposeCertificateAsset, uploaded("a.ttf", "font/ttf", []byte("OTTO"))); !errors.Is(err, media.ErrTypeNotAllowed) {
		t.Fatalf("font as a certificate asset: err = %v", err)
	}
}
