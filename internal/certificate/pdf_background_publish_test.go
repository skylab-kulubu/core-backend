package certificate_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/certificate"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/transit"
	"github.com/skylab-kulubu/core-backend/internal/transit/transittest"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// gotenbergContract answers like Gotenberg 8.37 (deploy/compose.yaml) on the
// two routes a PDF background uses. Its form checks are Gotenberg's own:
// watermarkOptions is read as a map of strings
// (pdfengines.formDataPdfStampsOrWatermarks), so a number in it is a 400,
// and a pdf watermark needs a file under the field "watermark".
type gotenbergContract struct {
	pages          int
	status         int  // of the convert route, when not 0
	metadataStatus int  // of the metadata route, when not 0
	unreachable    bool // the server is closed before core calls it
	converted      atomic.Int32
	options        atomic.Value
	watermark      atomic.Value // the last PDF laid under a page
}

func (g *gotenbergContract) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}
		switch r.URL.Path {
		case "/forms/pdfengines/metadata/read":
			if g.metadataStatus != 0 {
				http.Error(w, "Bad Gateway", g.metadataStatus)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]map[string]any{
				"background.pdf": {"PageCount": g.pages, "MIMEType": "application/pdf"},
			})
		case "/forms/chromium/convert/html":
			if r.FormValue("watermarkSource") != "" {
				var opts map[string]string
				if raw := r.FormValue("watermarkOptions"); raw != "" {
					if err := json.Unmarshal([]byte(raw), &opts); err != nil {
						http.Error(w, "Invalid form data: form field 'watermarkOptions' is invalid", http.StatusBadRequest)
						return
					}
				}
				if len(r.MultipartForm.File["watermark"]) != 1 {
					http.Error(w, "Invalid form data: a watermark file is required for image or pdf source", http.StatusBadRequest)
					return
				}
				g.options.Store(opts)
				if file, err := r.MultipartForm.File["watermark"][0].Open(); err == nil {
					laid, _ := io.ReadAll(file)
					_ = file.Close()
					g.watermark.Store(laid)
				}
			}
			if g.status != 0 {
				http.Error(w, "Service Unavailable", g.status)
				return
			}
			g.converted.Add(1)
			w.Header().Set("Content-Type", "application/pdf")
			_, _ = io.WriteString(w, "%PDF-1.7 composed")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// onePagePDF is a real one-page PDF, as an uploaded background is.
func onePagePDF() []byte {
	return []byte("%PDF-1.4\n1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n" +
		"2 0 obj\n<< /Type /Pages /Kids [3 0 R] /Count 1 >>\nendobj\n" +
		"3 0 obj\n<< /Type /Page /Parent 2 0 R /MediaBox [0 0 842 595] >>\nendobj\n" +
		"trailer\n<< /Root 1 0 R >>\n%%EOF\n")
}

type pdfBackgroundCertificates struct {
	mediaStore *media.MemoryStore
	public     *media.MemoryBlob
	storage    *media.PrivateStorage
	service    certificate.Service
	admin      authz.Principal
}

func newPDFBackgroundCertificates(t *testing.T, gotenberg *gotenbergContract) pdfBackgroundCertificates {
	t.Helper()
	srv := gotenberg.server(t)
	if gotenberg.unreachable {
		srv.Close()
	}
	az := authz.NewAuthorizer(authz.DefaultPolicy())
	mediaStore := media.NewMemoryStore()
	public, private := media.NewMemoryBlob(), media.NewMemoryBlob()
	storage := media.NewPrivateStorage(private, transit.New(transittest.NewServer(t).Config()))
	store := certificate.NewMemoryStore()
	render := &certificate.Gotenberg{BaseURL: srv.URL, HTTP: srv.Client()}
	service := certificate.NewServiceWithOptions(store, ticket.NewMemoryStore(), event.NewMemoryStore(), user.NewMemoryStore(), az, render, nil,
		certificate.Options{
			Templates: store, Artifacts: public, PrivateArtifacts: storage, Media: media.NewLinker(mediaStore),
			Assets: certificate.MediaAssets{Media: mediaStore, Blobs: public},
		})
	return pdfBackgroundCertificates{
		mediaStore: mediaStore, public: public, storage: storage, service: service,
		admin: authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/YK"}},
	}
}

// publicPDF is a PDF background as the panel uploads it today: legacy,
// public Media.
func (c pdfBackgroundCertificates) publicPDF(t *testing.T, pdf []byte) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	if err := c.public.Put(ctx, "files/background.pdf", pdf, media.ServingMetadata("application/pdf", "")); err != nil {
		t.Fatal(err)
	}
	created, err := c.mediaStore.Create(ctx, media.Media{
		Name: "background.pdf", Type: "application/pdf", Kind: media.KindFile, Key: "files/background.pdf", UploadedBy: uuid.New(),
		Purpose: media.PurposeLegacy, Visibility: media.VisibilityPublic,
	})
	if err != nil {
		t.Fatal(err)
	}
	return created.ID
}

// sealedPDF is a PDF background uploaded as a private certificate asset.
func (c pdfBackgroundCertificates) sealedPDF(t *testing.T, pdf []byte) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	sealed, err := c.storage.Seal(ctx, media.PrivateObjectKey("files/background"), pdf)
	if err != nil {
		t.Fatal(err)
	}
	created, err := c.mediaStore.Create(ctx, media.Media{
		Name: "background.pdf", Type: "application/pdf", Kind: media.KindFile, Key: sealed.Key, UploadedBy: uuid.New(),
		Purpose: media.PurposeCertificateAsset, Visibility: media.VisibilityPrivate, Encryption: &sealed.Encryption,
	})
	if err != nil {
		t.Fatal(err)
	}
	return created.ID
}

// The template of the report: a PDF background, the recipient's name, the
// Event's name, the verification QR and the serial.
func (c pdfBackgroundCertificates) template(t *testing.T, background uuid.UUID) certificate.Template {
	t.Helper()
	layout := certificate.Layout{
		Width: 1123, Height: 794, Orientation: "landscape", BackgroundMediaID: &background,
		Elements: []certificate.Element{
			{ID: "name", Kind: "recipientName", X: 100, Y: 300, Width: 900, Height: 80, FontSize: 48},
			{ID: "event", Kind: "eventName", X: 100, Y: 400, Width: 900, Height: 60, FontSize: 28},
			{ID: "qr", Kind: "verificationQr", X: 950, Y: 620, Width: 130, Height: 130},
			{ID: "serial", Kind: "serial", X: 100, Y: 700, Width: 400, Height: 30, FontSize: 12},
		},
	}
	template, err := c.service.CreateTemplate(context.Background(), c.admin, certificate.TemplateDraft{Name: "PDF", OwnerTeam: "WEBLAB", SourceKind: "upload", Layout: layout})
	if err != nil {
		t.Fatal(err)
	}
	return template
}

// A template on a one-page PDF background publishes, whether the PDF is
// public or a sealed private copy. Before the fix Gotenberg refused the
// numeric watermarkOptions with a 400, and publish answered a bare 400.
func TestPublishTemplateWithPDFBackground(t *testing.T) {
	t.Parallel()
	for name, upload := range map[string]func(pdfBackgroundCertificates, *testing.T, []byte) uuid.UUID{
		"public":  pdfBackgroundCertificates.publicPDF,
		"private": pdfBackgroundCertificates.sealedPDF,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			gotenberg := &gotenbergContract{pages: 1}
			c := newPDFBackgroundCertificates(t, gotenberg)
			template := c.template(t, upload(c, t, onePagePDF()))
			ctx := context.Background()

			if _, err := c.service.PreviewTemplate(ctx, c.admin, template.ID, certificate.PreviewData{}); err != nil {
				t.Fatalf("preview: %v", err)
			}
			version, err := c.service.PublishTemplate(ctx, c.admin, template.ID)
			if err != nil {
				t.Fatalf("publish: %v", err)
			}
			if version.ID == uuid.Nil || gotenberg.converted.Load() != 2 {
				t.Fatalf("version %v, converted %d", version.ID, gotenberg.converted.Load())
			}
			// The background covers the page, upright and opaque.
			want := map[string]string{"scale": "1", "opacity": "1", "rotation": "0"}
			got, _ := gotenberg.options.Load().(map[string]string)
			if len(got) != len(want) {
				t.Fatalf("watermarkOptions = %v", got)
			}
			for k, v := range want {
				if got[k] != v {
					t.Fatalf("watermarkOptions = %v, want %v", got, want)
				}
			}
		})
	}
}

// What a PDF background cannot be is said by code, not a bare 400.
func TestPublishTemplateWithPDFBackgroundSaysWhy(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		gotenberg *gotenbergContract
		pdf       []byte
		code      string
	}{
		{"two pages", &gotenbergContract{pages: 2}, onePagePDF(), certificate.CodeBackgroundPDFPages},
		{"not a PDF", &gotenbergContract{pages: 1}, []byte("GIF89a not a pdf"), certificate.CodeBackgroundPDFUnreadable},
		{"renderer refuses the page", &gotenbergContract{pages: 1, status: http.StatusBadRequest}, onePagePDF(), certificate.CodeRenderFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := newPDFBackgroundCertificates(t, tc.gotenberg)
			template := c.template(t, c.publicPDF(t, tc.pdf))
			_, err := c.service.PublishTemplate(context.Background(), c.admin, template.ID)
			var refused *certificate.InvalidError
			if !errors.As(err, &refused) || refused.Code != tc.code || !errors.Is(err, certificate.ErrInvalid) {
				t.Fatalf("err = %v, want code %s", err, tc.code)
			}
			if tc.gotenberg.converted.Load() != 0 {
				t.Fatal("a refused background was rendered")
			}
		})
	}
}

// A renderer that is down, unreachable or failing on its side (5xx) is
// unavailable, not an invalid template: 503 with renderer_unavailable, and
// an issuance job tries it again (it is not ErrInvalid).
func TestPublishTemplateWhileTheRendererIsDown(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		gotenberg *gotenbergContract
	}{
		{"metadata answers 502", &gotenbergContract{pages: 1, metadataStatus: http.StatusBadGateway}},
		{"convert answers 503", &gotenbergContract{pages: 1, status: http.StatusServiceUnavailable}},
		{"unreachable", &gotenbergContract{pages: 1, unreachable: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := newPDFBackgroundCertificates(t, tc.gotenberg)
			template := c.template(t, c.publicPDF(t, onePagePDF()))
			_, err := c.service.PublishTemplate(context.Background(), c.admin, template.ID)
			var unavailable *certificate.UnavailableError
			if !errors.As(err, &unavailable) || unavailable.Code != certificate.CodeRendererUnavailable {
				t.Fatalf("err = %v, want %s", err, certificate.CodeRendererUnavailable)
			}
			if errors.Is(err, certificate.ErrInvalid) {
				t.Fatal("an unavailable renderer must not read as an invalid template: issuance would stop retrying")
			}
		})
	}
}
