package certificate_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

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

// placedCertificates is a certificate service with Gotenberg and, unless
// tools is nil, pdf-tools, both the contract fakes, over memory stores.
type placedCertificates struct {
	pdfBackgroundCertificates
	private *media.MemoryBlob
	store   *certificate.MemoryStore
	events  *event.MemoryStore
	tickets *ticket.MemoryStore
	users   *user.MemoryStore
	az      authz.Authorizer
}

func newPlacedCertificates(t *testing.T, gotenberg *gotenbergContract, tools *pdfToolsContract) placedCertificates {
	t.Helper()
	srv := gotenberg.server(t)
	az := authz.NewAuthorizer(authz.DefaultPolicy())
	mediaStore := media.NewMemoryStore()
	public, private := media.NewMemoryBlob(), media.NewMemoryBlob()
	storage := media.NewPrivateStorage(private, transit.New(transittest.NewServer(t).Config()))
	store := certificate.NewMemoryStore()
	events, tickets, users := event.NewMemoryStore(), ticket.NewMemoryStore(), user.NewMemoryStore()
	var pdfTools certificate.PDFTools
	if tools != nil {
		pdfTools = tools.client(t)
	}
	service := certificate.NewServiceWithOptions(store, tickets, events, users, az, &certificate.Gotenberg{BaseURL: srv.URL, HTTP: srv.Client()}, &recMail{},
		certificate.Options{
			PublicAPIOrigin: "https://api.example.test", VerifyOrigin: "https://skyl.app/c",
			Templates: store, Jobs: store, Artifacts: public, PrivateArtifacts: storage, Media: media.NewLinker(mediaStore),
			Assets: certificate.MediaAssets{Media: mediaStore, Blobs: public}, PDFTools: pdfTools,
		})
	return placedCertificates{
		pdfBackgroundCertificates: pdfBackgroundCertificates{
			mediaStore: mediaStore, public: public, storage: storage, service: service,
			admin: authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/YK"}},
		},
		private: private, store: store, events: events, tickets: tickets, users: users, az: az,
	}
}

// placedTemplate is the report's template with its PDF background placed:
// an A4 portrait PDF contained in an A4 landscape page.
func (c placedCertificates) placedTemplate(t *testing.T, background uuid.UUID, placement *certificate.BackgroundPlacement) certificate.Template {
	t.Helper()
	template := c.template(t, background)
	template.DraftLayout.BackgroundPlacement = placement
	updated, err := c.service.UpdateTemplate(context.Background(), c.admin, template.ID, certificate.TemplateDraft{
		Name: template.Name, OwnerTeam: "ARTLAB", SourceKind: "upload", Layout: template.DraftLayout,
	})
	if err != nil {
		t.Fatal(err)
	}
	return updated
}

var contain = &certificate.BackgroundPlacement{Fit: certificate.FitContain, X: 0.5, Y: 0.5, Scale: 1}

// Publishing a placed PDF background stores its placed copy beside the
// asset's (encrypted like it for a private asset) and names it in the
// manifest; the publish render prints that copy, so pdf-tools places it
// once.
func TestPublishStoresThePlacedCopy(t *testing.T) {
	t.Parallel()
	for name, upload := range map[string]func(pdfBackgroundCertificates, *testing.T, []byte) uuid.UUID{
		"public":  pdfBackgroundCertificates.publicPDF,
		"private": pdfBackgroundCertificates.sealedPDF,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			gotenberg := &gotenbergContract{pages: 1}
			tools := &pdfToolsContract{widthPt: 595.276, heightPt: 841.89, pages: 1}
			c := newPlacedCertificates(t, gotenberg, tools)
			background := upload(c.pdfBackgroundCertificates, t, onePagePDF())
			template := c.placedTemplate(t, background, contain)

			version, err := c.service.PublishTemplate(ctx, c.admin, template.ID)
			if err != nil {
				t.Fatalf("publish: %v", err)
			}
			ref := version.AssetManifest[background.String()]
			if ref.Placed == nil {
				t.Fatalf("manifest %+v has no placed copy", version.AssetManifest)
			}
			sum := sha256.Sum256([]byte(placedPDF))
			if ref.Placed.Checksum != hex.EncodeToString(sum[:]) || !strings.HasSuffix(ref.Placed.Key, version.ID.String()+"/"+background.String()+".placed.pdf") {
				t.Fatalf("placed ref %+v", ref.Placed)
			}
			var stored []byte
			if name == "private" {
				sealed, private := ref.Placed.Sealed()
				if !private || ref.Encryption == nil {
					t.Fatalf("a private asset's placed copy is not encrypted: %+v", ref)
				}
				if _, public := c.public.Get(ref.Placed.Key); public {
					t.Fatal("a private asset's placed copy is in the public bucket")
				}
				if stored, err = c.storage.Read(ctx, sealed); err != nil {
					t.Fatal(err)
				}
			} else {
				if ref.Placed.Encryption != nil {
					t.Fatalf("a public asset's placed copy is encrypted: %+v", ref.Placed)
				}
				stored, _ = c.public.Get(ref.Placed.Key)
			}
			if string(stored) != placedPDF {
				t.Fatalf("stored placed copy %q", stored)
			}
			laid, _ := gotenberg.watermark.Load().([]byte)
			if string(laid) != placedPDF || tools.placed.Load() != 1 || tools.inspected.Load() != 1 {
				t.Fatalf("laid %q, places %d, inspects %d", laid, tools.placed.Load(), tools.inspected.Load())
			}
			// The page is the layout's in pt; the A4 portrait PDF is
			// contained in it, centred.
			params := tools.places[0]
			rect, _ := params["rect"].(map[string]any)
			if params["pageWidthPt"] != 842.25 || params["pageHeightPt"] != 595.5 || !closeTo(rect["h"], 595.5) || !closeTo(rect["w"], 595.5*595.276/841.89) {
				t.Fatalf("place params %v", params)
			}
		})
	}
}

func closeTo(v any, want float64) bool {
	f, ok := v.(float64)
	return ok && f > want-0.01 && f < want+0.01
}

// pdf-tools off or down: publishing a placed PDF background is 503
// pdf_tools_unavailable and stores no version; a layout without a placement
// publishes as before without pdf-tools.
func TestPublishPlacedPDFNeedsPDFTools(t *testing.T) {
	t.Parallel()
	for name, tools := range map[string]*pdfToolsContract{
		"off":  nil,
		"busy": {pages: 1, status: http.StatusServiceUnavailable, code: "busy", retryAfter: "5"},
		"5xx":  {pages: 1, status: http.StatusInternalServerError, code: "internal"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			gotenberg := &gotenbergContract{pages: 1}
			c := newPlacedCertificates(t, gotenberg, tools)
			background := c.sealedPDF(t, onePagePDF())
			placed := c.placedTemplate(t, background, contain)
			before := len(c.private.Keys())

			_, err := c.service.PublishTemplate(ctx, c.admin, placed.ID)
			assertPDFToolsUnavailable(t, err)
			if got, _ := c.service.GetTemplate(ctx, c.admin, placed.ID); got.PublishedVersion != nil {
				t.Fatal("a failed publish stored a version")
			}
			if len(c.private.Keys()) != before {
				t.Fatalf("a failed publish left private copies: %v", c.private.Keys())
			}
			if tools != nil && tools.calls() > 2 {
				t.Fatalf("%d calls to pdf-tools for one publish", tools.calls())
			}
			if gotenberg.converted.Load() != 0 {
				t.Fatal("a placed background was printed without pdf-tools")
			}

			legacy := c.template(t, c.publicPDF(t, onePagePDF()))
			if _, err := c.service.PublishTemplate(ctx, c.admin, legacy.ID); err != nil {
				t.Fatalf("a layout without a placement needs no pdf-tools: %v", err)
			}
		})
	}
}

// What pdf-tools refuses of the PDF is said by code at publish.
func TestPublishPlacedPDFSaysWhy(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		tools *pdfToolsContract
		code  string
	}{
		"encrypted":  {&pdfToolsContract{widthPt: 595, heightPt: 842, pages: 1, encrypted: true}, certificate.CodeBackgroundPDFEncrypted},
		"refused":    {&pdfToolsContract{pages: 1, status: http.StatusUnprocessableEntity, code: "pdf_encrypted"}, certificate.CodeBackgroundPDFEncrypted},
		"two pages":  {&pdfToolsContract{widthPt: 595, heightPt: 842, pages: 2}, certificate.CodeBackgroundPDFPages},
		"unreadable": {&pdfToolsContract{pages: 1, status: http.StatusUnprocessableEntity, code: "pdf_unreadable"}, certificate.CodeBackgroundPDFUnreadable},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := newPlacedCertificates(t, &gotenbergContract{pages: 1}, tc.tools)
			template := c.placedTemplate(t, c.publicPDF(t, onePagePDF()), contain)
			_, err := c.service.PublishTemplate(context.Background(), c.admin, template.ID)
			var refused *certificate.InvalidError
			if !errors.As(err, &refused) || refused.Code != tc.code {
				t.Fatalf("err = %v, want %s", err, tc.code)
			}
		})
	}
}

// A certificate issued on a version with a placed PDF background prints
// the version's placed copy: issuance never calls pdf-tools, and works
// while pdf-tools is down.
func TestIssuancePrintsThePlacedCopyWithoutPDFTools(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	gotenberg := &gotenbergContract{pages: 1}
	tools := &pdfToolsContract{widthPt: 595.276, heightPt: 841.89, pages: 1}
	c := newPlacedCertificates(t, gotenberg, tools)
	template := c.placedTemplate(t, c.sealedPDF(t, onePagePDF()), contain)
	if _, err := c.service.PublishTemplate(ctx, c.admin, template.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := c.store.SetBinding(ctx, certificate.Binding{ID: uuid.New(), Scope: certificate.ScopeOwnerTeam, ScopeKey: "ARTLAB", TemplateID: template.ID}); err != nil {
		t.Fatal(err)
	}
	callsAtPublish := tools.calls()
	// pdf-tools goes down after the publish.
	tools.down.Store(true)

	start, end := time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour)
	ev, err := c.events.Create(ctx, event.Event{Name: "Yerleşim Günü", Location: "YTÜ", OwnerTeam: "ARTLAB", AttendanceRule: certificate.RuleOnce, StartDate: &start, EndDate: &end})
	if err != nil {
		t.Fatal(err)
	}
	day, _ := c.events.CreateDay(ctx, event.Day{EventID: ev.ID, Name: "Gün"})
	session, _ := c.events.CreateSession(ctx, event.Session{EventDayID: day.ID, Title: "Oturum", CertificateCounted: true, SpeakerName: "Ada", SessionType: "PRESENTATION"})
	owner := uuid.New()
	if _, _, err := c.users.Upsert(ctx, user.User{ID: owner, Email: "ada@example.com", FirstName: "Ada", LastName: "Lovelace"}); err != nil {
		t.Fatal(err)
	}
	tickets := ticket.NewService(c.tickets, c.events, c.az, c.users)
	item, err := tickets.Apply(ctx, authz.Principal{ID: owner.String()}, ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	leader := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/ARTLAB/LIDERLER"}}
	if _, err := tickets.CheckIn(ctx, leader, item.ID, session.ID); err != nil {
		t.Fatal(err)
	}
	// The Event ends: its attendance closes and the certificate is queued.
	ended := time.Now().Add(-time.Minute)
	ev.EndDate = &ended
	if ev, err = c.events.Update(ctx, ev); err != nil {
		t.Fatal(err)
	}
	if _, err := c.service.Finalize(ctx, leader, ev.ID); err != nil {
		t.Fatal(err)
	}
	gotenberg.watermark.Store([]byte(nil))
	if processed, err := c.service.ProcessNext(ctx, 10); err != nil || processed != 1 {
		t.Fatalf("processed %d, err %v", processed, err)
	}
	listed, err := c.service.ListByEvent(ctx, leader, ev.ID)
	if err != nil || len(listed) != 1 {
		t.Fatalf("issued %v %+v", err, listed)
	}
	laid, _ := gotenberg.watermark.Load().([]byte)
	if !bytes.Equal(laid, []byte(placedPDF)) || tools.calls() != callsAtPublish {
		t.Fatalf("laid %q, pdf-tools calls %d (at publish %d)", laid, tools.calls(), callsAtPublish)
	}
}

// GET /v1/certificate-assets/{id}/info and …/preview: a PDF's page in px
// (pt × 96/72) and its PNG from pdf-tools, an image's own size and bytes;
// for who uploaded it or may edit a template that links it.
func TestCertificateAssetInfoAndPreview(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tools := &pdfToolsContract{widthPt: 595.276, heightPt: 841.89, pages: 1, textCount: 12}
	c := newPlacedCertificates(t, &gotenbergContract{pages: 1}, tools)
	pdfID := c.sealedPDF(t, onePagePDF())
	pdfMedia, _ := c.mediaStore.Get(ctx, pdfID)
	uploader := authz.Principal{ID: pdfMedia.UploadedBy.String()}

	info, err := c.service.AssetInfo(ctx, uploader, pdfID)
	if err != nil {
		t.Fatal(err)
	}
	if info.ContentType != "application/pdf" || info.Pages != 1 || info.TextCount != 12 ||
		!closeTo(info.WidthPx, 595.276*96/72) || !closeTo(info.HeightPx, 841.89*96/72) {
		t.Fatalf("info %+v", info)
	}
	preview, err := c.service.AssetPreview(ctx, uploader, pdfID, 1200)
	if err != nil || preview.ContentType != "image/png" || !bytes.HasPrefix(preview.Data, []byte("\x89PNG")) || tools.renderParams[0]["maxPx"] != 1200.0 {
		t.Fatalf("preview %v %q %v", err, preview.ContentType, tools.renderParams)
	}
	if _, err := c.service.AssetPreview(ctx, uploader, pdfID, 5000); !errors.Is(err, certificate.ErrInvalid) {
		t.Fatalf("max over 4000: %v", err)
	}

	// Another member may not read it, until a template they may edit links
	// it; another product's Media is not found.
	stranger := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
	if _, err := c.service.AssetInfo(ctx, stranger, pdfID); !errors.Is(err, certificate.ErrForbidden) {
		t.Fatalf("stranger: %v", err)
	}
	c.template(t, pdfID) // a WEBLAB template
	if _, err := c.service.AssetInfo(ctx, stranger, pdfID); err != nil {
		t.Fatalf("a WEBLAB leader who may edit the template: %v", err)
	}
	other := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/ARTLAB/LIDERLER"}}
	if _, err := c.service.AssetInfo(ctx, other, pdfID); !errors.Is(err, certificate.ErrForbidden) {
		t.Fatalf("another team: %v", err)
	}
	answer, err := c.mediaStore.Create(ctx, media.Media{Name: "a.pdf", Type: "application/pdf", Kind: media.KindFile, Key: "files/a", UploadedBy: uuid.New(), Purpose: media.PurposeAnswerFile})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.service.AssetInfo(ctx, c.admin, answer.ID); !errors.Is(err, certificate.ErrNotFound) {
		t.Fatalf("an Answer file: %v", err)
	}

	// An image: its own size and bytes, without pdf-tools.
	png := pngDot()
	if err := c.public.Put(ctx, "images/dot.png", png, media.ServingMetadata("image/png", "")); err != nil {
		t.Fatal(err)
	}
	image, err := c.mediaStore.Create(ctx, media.Media{Name: "dot.png", Type: "image/png", Kind: media.KindImage, Key: "images/dot.png", UploadedBy: uuid.MustParse(uploader.ID), Purpose: media.PurposeLegacy})
	if err != nil {
		t.Fatal(err)
	}
	calls := tools.calls()
	info, err = c.service.AssetInfo(ctx, uploader, image.ID)
	if err != nil || info.ContentType != "image/png" || info.WidthPx <= 0 || info.Pages != 1 {
		t.Fatalf("image info %+v %v", info, err)
	}
	preview, err = c.service.AssetPreview(ctx, uploader, image.ID, certificate.DefaultAssetPreviewPx)
	if err != nil || preview.ContentType != "image/png" || !bytes.Equal(preview.Data, png) || tools.calls() != calls {
		t.Fatalf("image preview %v %q", err, preview.ContentType)
	}
}

// Without pdf-tools a PDF asset's info and preview are 503
// pdf_tools_unavailable; an image's still answer.
func TestCertificateAssetPDFWithoutPDFTools(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := newPlacedCertificates(t, &gotenbergContract{pages: 1}, nil)
	pdfID := c.publicPDF(t, onePagePDF())
	uploaded, _ := c.mediaStore.Get(ctx, pdfID)
	uploader := authz.Principal{ID: uploaded.UploadedBy.String()}
	_, err := c.service.AssetInfo(ctx, uploader, pdfID)
	assertPDFToolsUnavailable(t, err)
	_, err = c.service.AssetPreview(ctx, uploader, pdfID, 2000)
	assertPDFToolsUnavailable(t, err)
}
