package certificate_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
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

// textToolsContract is pdf-tools' /v1/inspect (with text) and
// /v1/strip-text as pdf-tools/README.md describes them.
type textToolsContract struct {
	inspect     map[string]any // /v1/inspect's answer
	stripStatus int            // of /v1/strip-text, when not 0
	stripCode   string
	stripped    []byte

	mu       sync.Mutex
	inspects int
	strips   [][]string
}

func (c *textToolsContract) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		problem := func(status int, code string) {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]any{"status": status, "code": code})
		}
		if r.Method != http.MethodPost || r.ContentLength <= 0 || r.ParseMultipartForm(32<<20) != nil {
			problem(http.StatusBadRequest, "bad_request")
			return
		}
		file, _, err := r.FormFile("file")
		if err != nil {
			problem(http.StatusBadRequest, "bad_request")
			return
		}
		pdf, _ := io.ReadAll(file)
		if !bytes.HasPrefix(pdf, []byte("%PDF-")) {
			problem(http.StatusUnprocessableEntity, "pdf_unreadable")
			return
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		switch r.URL.Path {
		case "/v1/inspect":
			c.inspects++
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(c.inspect)
		case "/v1/strip-text":
			var params struct {
				SpanIDs []string `json:"spanIds"`
			}
			if json.Unmarshal([]byte(r.FormValue("params")), &params) != nil || len(params.SpanIDs) == 0 {
				problem(http.StatusBadRequest, "bad_request")
				return
			}
			c.strips = append(c.strips, params.SpanIDs)
			if c.stripStatus != 0 {
				problem(c.stripStatus, c.stripCode)
				return
			}
			w.Header().Set("Content-Type", "application/pdf")
			_, _ = w.Write(c.stripped)
		default:
			problem(http.StatusNotFound, "not_found")
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func contractSpan(id, text string, x, y, size, width float64, font map[string]any) map[string]any {
	return map[string]any{
		"id": id, "text": text, "angle": 0, "size": size, "color": "#1a2659", "opacity": 1, "renderMode": 0,
		"origin": map[string]any{"x": x, "y": y},
		"quad":   [][2]float64{{x, y + 0.2*size}, {x + width, y + 0.2*size}, {x + width, y - 0.8*size}, {x, y - 0.8*size}},
		"font":   font, "convertible": true,
	}
}

// certificateInspect is /v1/inspect of a Canva-like A4 landscape
// certificate: a Turkish title, a placeholder name in two spans, a line
// drawn as an outline (not convertible).
func certificateInspect() map[string]any {
	montserrat := map[string]any{"name": "Montserrat-Bold", "family": "Montserrat", "weight": 700, "ascent": 0.968, "descent": -0.251}
	brandon := map[string]any{"name": "BrandonGrotesque-Regular", "weight": 400, "ascent": 0.8, "descent": -0.2}
	outline := contractSpan("7", "Kontur", 300, 500, 18, 80, brandon)
	outline["convertible"], outline["reason"], outline["renderMode"] = false, "stroke", 1
	return map[string]any{
		"pages": 1, "page": map[string]any{"widthPt": 842, "heightPt": 595, "rotate": 0}, "encrypted": false, "textCount": 4, "truncated": false,
		"objects": map[string]any{"text": 4, "path": 12, "image": 1, "shading": 1, "form": 0},
		"spans": []map[string]any{
			contractSpan("3", "Katılım Belgesi", 220, 166, 40, 300, montserrat),
			contractSpan("5", "Ad", 300, 315, 32, 40, brandon),
			contractSpan("6", "Soyad", 350, 315, 32, 90, brandon),
			outline,
		},
		"runs": []map[string]any{
			{"spanIds": []string{"3"}, "text": "Katılım Belgesi", "line": 0, "block": 0},
			{"spanIds": []string{"5", "6"}, "text": "Ad Soyad", "line": 1, "block": 1},
			{"spanIds": []string{"7"}, "text": "Kontur", "line": 2, "block": 2},
		},
	}
}

type textCertificates struct {
	service  certificate.Service
	media    *media.MemoryStore
	public   *media.MemoryBlob
	storage  *media.PrivateStorage
	editor   authz.Principal
	original uuid.UUID
	tools    *textToolsContract
}

func newTextCertificates(t *testing.T, tools *textToolsContract, withTools, withUploads bool) textCertificates {
	t.Helper()
	bao := transittest.NewServer(t)
	az := authz.NewAuthorizer(authz.DefaultPolicy())
	mediaStore := media.NewMemoryStore()
	public, private := media.NewMemoryBlob(), media.NewMemoryBlob()
	storage := media.NewPrivateStorage(private, transit.New(bao.Config()))
	catalogue, err := media.LoadCatalogue()
	if err != nil {
		t.Fatal(err)
	}
	mediaSvc := media.NewServiceWithOptions(mediaStore, public, az, "https://cdn.example.test", media.ServiceOptions{
		Catalogue: catalogue,
		Private: &media.PrivateMedia{
			Storage: storage, LinkKey: bytes.Repeat([]byte{7}, 32), LinkOrigin: "https://api.example.test/", AccessLog: mediaStore, Now: bao.Clock.Now,
		},
	})
	store := certificate.NewMemoryStore()
	opts := certificate.Options{
		Templates: store, Artifacts: public, PrivateArtifacts: storage, Media: media.NewLinker(mediaStore),
		Assets: certificate.MediaAssets{Media: mediaStore, Blobs: public},
	}
	if withTools {
		srv := tools.server(t)
		opts.PDFTools = &certificate.PDFToolsClient{BaseURL: srv.URL, HTTP: srv.Client(), MaxBusyWait: time.Millisecond}
	}
	if withUploads {
		opts.Uploads = mediaSvc
	}
	service := certificate.NewServiceWithOptions(store, ticket.NewMemoryStore(), event.NewMemoryStore(), user.NewMemoryStore(), az, staticRenderer{}, nil, opts)
	editor := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/YK"}}
	ctx := context.Background()
	sealed, err := storage.Seal(ctx, media.PrivateObjectKey("files/canva"), onePagePDF())
	if err != nil {
		t.Fatal(err)
	}
	created, err := mediaStore.Create(ctx, media.Media{
		Name: "katilim-belgesi.pdf", Type: "application/pdf", Kind: media.KindFile, Key: sealed.Key, UploadedBy: uuid.MustParse(editor.ID),
		Purpose: media.PurposeCertificateAsset, Visibility: media.VisibilityPrivate, Encryption: &sealed.Encryption,
	})
	if err != nil {
		t.Fatal(err)
	}
	return textCertificates{service: service, media: mediaStore, public: public, storage: storage, editor: editor, original: created.ID, tools: tools}
}

// staticRenderer is a renderer the text routes never call.
type staticRenderer struct{}

func (staticRenderer) PDF(context.Context, string) ([]byte, error) {
	return nil, errors.New("not rendered")
}

func conversionRequest(ids ...string) certificate.TextConversionRequest {
	req := certificate.TextConversionRequest{SpanIDs: ids}
	req.Page.Width, req.Page.Height = 1123, 794
	return req
}

func refusedWith(t *testing.T, err error, code string) {
	t.Helper()
	var refused *certificate.InvalidError
	if !errors.As(err, &refused) || refused.Code != code {
		t.Fatalf("got %v, want %s", err, code)
	}
}

// The text list: one piece per run, the font each is set in, the outline
// text listed with its reason; positions are left out.
func TestAssetTextLists(t *testing.T) {
	t.Parallel()
	tools := &textToolsContract{inspect: certificateInspect()}
	c := newTextCertificates(t, tools, true, true)
	text, err := c.service.AssetText(context.Background(), c.editor, c.original)
	if err != nil {
		t.Fatal(err)
	}
	if len(text.Spans) != 3 || text.TextMissing != "" || text.Truncated {
		t.Fatalf("text %+v", text)
	}
	want := []certificate.AssetTextSpan{
		{ID: "3", Text: "Katılım Belgesi", SourceFont: "Montserrat-Bold", Font: "Montserrat", FontWeight: 700, FontSizePt: 40, Convertible: true},
		{ID: "5", Text: "Ad Soyad", SourceFont: "BrandonGrotesque-Regular", Font: "Noto Sans", FontWeight: 400, FontSubstituted: true, FontSizePt: 32, Convertible: true},
		{ID: "7", Text: "Kontur", SourceFont: "BrandonGrotesque-Regular", Font: "Noto Sans", FontWeight: 400, FontSubstituted: true, FontSizePt: 18, Reason: "stroke"},
	}
	for i := range want {
		if text.Spans[i] != want[i] {
			t.Fatalf("span %d %+v, want %+v", i, text.Spans[i], want[i])
		}
	}
}

// A PDF without text says why: outlines (shapes, its text turned into
// curves), a picture, or unknown (a pdf-tools that does not count).
func TestAssetTextSaysWhyThereIsNone(t *testing.T) {
	t.Parallel()
	for want, objects := range map[string]any{
		"outlines": map[string]any{"text": 0, "path": 240, "image": 0, "shading": 0, "form": 0},
		"image":    map[string]any{"text": 0, "path": 2, "image": 1, "shading": 0, "form": 0},
		"unknown":  nil,
	} {
		t.Run(want, func(t *testing.T) {
			t.Parallel()
			inspect := certificateInspect()
			inspect["spans"], inspect["runs"], inspect["textCount"] = []any{}, []any{}, 0
			if objects == nil {
				delete(inspect, "objects")
			} else {
				inspect["objects"] = objects
			}
			c := newTextCertificates(t, &textToolsContract{inspect: inspect}, true, true)
			text, err := c.service.AssetText(context.Background(), c.editor, c.original)
			if err != nil || len(text.Spans) != 0 || text.TextMissing != want {
				t.Fatalf("text %+v, %v", text, err)
			}
			_, err = c.service.ConvertAssetText(context.Background(), c.editor, c.original, conversionRequest("3"))
			refusedWith(t, err, certificate.CodeBackgroundPDFNoText)
		})
	}
}

// A conversion: pdf-tools strips the chosen runs' spans, the PDF it
// answers becomes a new private, encrypted, pending certificate asset of
// the caller's, and the elements come back for the draft, which links
// both the new background and the original.
func TestConvertAssetText(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	stripped := []byte("%PDF-1.7 without the converted text")
	tools := &textToolsContract{inspect: certificateInspect(), stripped: stripped}
	c := newTextCertificates(t, tools, true, true)
	req := conversionRequest("3", "5")
	req.Kinds = map[string]string{"5": "recipientName"}
	req.Placement = &certificate.BackgroundPlacement{Fit: certificate.FitFill, X: 0.5, Y: 0.5, Scale: 1}
	conversion, err := c.service.ConvertAssetText(ctx, c.editor, c.original, req)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(tools.strips[0])
	if len(tools.strips) != 1 || len(tools.strips[0]) != 3 || tools.strips[0][0] != "3" || tools.strips[0][2] != "6" {
		t.Fatalf("strip-text got %v", tools.strips)
	}
	if conversion.BackgroundOriginalMediaID != c.original || conversion.BackgroundMediaID == c.original || len(conversion.Elements) != 2 {
		t.Fatalf("conversion %+v", conversion)
	}
	title, name := conversion.Elements[0], conversion.Elements[1]
	if title.Kind != "staticText" || title.Text != "Katılım Belgesi" || title.FontFamily != "Montserrat" || title.Origin != certificate.OriginBackgroundText ||
		name.Kind != "recipientName" || name.Text != "" || name.SourceFont != "BrandonGrotesque-Regular" || len(name.ID) != len("bgtext-")+36 {
		t.Fatalf("elements %+v", conversion.Elements)
	}
	if len(conversion.Warnings) != 1 || conversion.Warnings[0].Kind != certificate.WarningFontSubstituted || conversion.Warnings[0].ElementID != name.ID ||
		conversion.Warnings[0].SpanID != "5" || conversion.Warnings[0].To != "Noto Sans" {
		t.Fatalf("warnings %+v", conversion.Warnings)
	}
	created, err := c.media.Get(ctx, conversion.BackgroundMediaID)
	if err != nil {
		t.Fatal(err)
	}
	if created.Purpose != media.PurposeCertificateAsset || created.Visibility != media.VisibilityPrivate || created.Encryption == nil ||
		created.UploadedBy.String() != c.editor.ID || created.Status != media.StatusPending || created.ExpiresAt == nil ||
		created.ExpiresAt.Sub(time.Now()) > 25*time.Hour || created.Type != "application/pdf" || created.Name != "katilim-belgesi-metinsiz.pdf" {
		t.Fatalf("new background %+v", created)
	}
	sealed, _ := created.Sealed()
	if data, err := c.storage.Read(ctx, sealed); err != nil || !bytes.Equal(data, stripped) {
		t.Fatalf("stored %q, %v", data, err)
	}
	if _, public := c.public.Get(created.Key); public {
		t.Fatal("the new background is in the public bucket")
	}

	// The editor applies it to its draft and saves it.
	layout := certificate.Layout{
		Width: 1123, Height: 794, Orientation: "landscape", BackgroundMediaID: &conversion.BackgroundMediaID,
		BackgroundOriginalMediaID: &conversion.BackgroundOriginalMediaID, BackgroundPlacement: req.Placement,
		Elements: append(conversion.Elements, certificate.Element{ID: "qr", Kind: "verificationQr", X: 950, Y: 620, Width: 130, Height: 130}),
	}
	if _, err := c.service.CreateTemplate(ctx, c.editor, certificate.TemplateDraft{Name: "Canva", OwnerTeam: "WEBLAB", SourceKind: "upload", Layout: layout}); err != nil {
		t.Fatalf("save the converted draft: %v", err)
	}
}

// The refusals: nothing (or nothing convertible, or an unknown piece)
// chosen; pdf-tools not removing the text, or refusing the ids; too many
// elements; no pdf-tools; no storage for the new PDF; someone else's PDF;
// a malformed request.
func TestConvertAssetTextRefusals(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ok := []byte("%PDF-1.7 stripped")
	for name, tc := range map[string]struct {
		tools       *textToolsContract
		noTools     bool
		noUploads   bool
		req         certificate.TextConversionRequest
		someoneElse bool
		code        string
		err         error
		unavailable bool
	}{
		"nothing chosen":        {req: conversionRequest(), code: certificate.CodeBackgroundTextSelectionEmpty},
		"not convertible":       {req: conversionRequest("3", "7"), code: certificate.CodeBackgroundTextSelectionEmpty},
		"unknown piece":         {req: conversionRequest("99"), code: certificate.CodeBackgroundTextSelectionEmpty},
		"a span, not a run":     {req: conversionRequest("6"), code: certificate.CodeBackgroundTextSelectionEmpty},
		"strip incomplete":      {tools: &textToolsContract{stripStatus: http.StatusUnprocessableEntity, stripCode: "strip_incomplete"}, req: conversionRequest("3"), code: certificate.CodeBackgroundTextStripFailed},
		"strip refuses the ids": {tools: &textToolsContract{stripStatus: http.StatusBadRequest, stripCode: "bad_request"}, req: conversionRequest("3"), code: certificate.CodeBackgroundTextSelectionEmpty},
		"strip answers no PDF":  {tools: &textToolsContract{stripped: []byte("<html>")}, req: conversionRequest("3"), unavailable: true},
		"too many elements": {req: func() certificate.TextConversionRequest {
			r := conversionRequest("3", "5")
			r.ExistingElements = 99
			return r
		}(), code: certificate.CodeLayoutElementCount},
		"no pdf-tools": {noTools: true, req: conversionRequest("3"), unavailable: true},
		"no storage":   {noUploads: true, req: conversionRequest("3"), code: certificate.CodeAssetStorageUnavailable},
		"someone else": {someoneElse: true, req: conversionRequest("3"), err: certificate.ErrNotFound},
		"bad kind": {req: func() certificate.TextConversionRequest {
			r := conversionRequest("3")
			r.Kinds = map[string]string{"3": "serial"}
			return r
		}(), err: certificate.ErrInvalid},
		"kind unchosen": {req: func() certificate.TextConversionRequest {
			r := conversionRequest("3")
			r.Kinds = map[string]string{"5": "eventName"}
			return r
		}(), err: certificate.ErrInvalid},
		"bad page": {req: func() certificate.TextConversionRequest { r := conversionRequest("3"); r.Page.Width = 100; return r }(),
			code: certificate.CodeLayoutSizeInvalid},
		"bad placement": {req: func() certificate.TextConversionRequest {
			r := conversionRequest("3")
			r.Placement = &certificate.BackgroundPlacement{Fit: "stretch", Scale: 1}
			return r
		}(), code: certificate.CodeLayoutPlacementInvalid},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			tools := tc.tools
			if tools == nil {
				tools = &textToolsContract{stripped: ok}
			}
			tools.inspect = certificateInspect()
			c := newTextCertificates(t, tools, !tc.noTools, !tc.noUploads)
			who := c.editor
			if tc.someoneElse {
				who = authz.Principal{ID: uuid.NewString()}
			}
			_, err := c.service.ConvertAssetText(ctx, who, c.original, tc.req)
			switch {
			case tc.code != "":
				refusedWith(t, err, tc.code)
			case tc.unavailable:
				var unavailable *certificate.UnavailableError
				if !errors.As(err, &unavailable) || unavailable.Code != certificate.CodePDFToolsUnavailable {
					t.Fatalf("got %v, want pdf_tools_unavailable", err)
				}
			default:
				if !errors.Is(err, tc.err) {
					t.Fatalf("got %v, want %v", err, tc.err)
				}
			}
			// Nothing is stored for a refused conversion.
			if items, _ := c.media.List(ctx); len(items) != 1 {
				t.Fatalf("%d Media after a refusal", len(items))
			}
		})
	}
}
