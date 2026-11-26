package certificate_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

// fontToolsContract is pdf-tools' /v1/font/sanitize as pdf-tools/README.md
// describes it; every other path goes to text (when set).
type fontToolsContract struct {
	text   *textToolsContract
	status int // a problem instead of the answer, when not 0
	code   string
	family string
	weight int
	italic bool
	// missing is missingTurkish.
	missing string
	woff2   []byte

	mu    sync.Mutex
	calls [][]byte
}

func (c *fontToolsContract) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/font/sanitize" {
			if c.text == nil {
				http.NotFound(w, r)
				return
			}
			c.text.handler()(w, r)
			return
		}
		problem := func(status int, code string) {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]any{"type": "about:blank", "title": http.StatusText(status), "status": status, "code": code})
		}
		if r.Method != http.MethodPost || r.ParseMultipartForm(32<<20) != nil {
			problem(http.StatusBadRequest, "bad_request")
			return
		}
		file, _, err := r.FormFile("file")
		if err != nil {
			problem(http.StatusBadRequest, "bad_request")
			return
		}
		data, _ := io.ReadAll(file)
		c.mu.Lock()
		c.calls = append(c.calls, data)
		c.mu.Unlock()
		if c.status != 0 {
			problem(c.status, c.code)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"family": c.family, "subfamily": "Regular", "weight": c.weight, "italic": c.italic, "glyphCount": 412,
			"missingTurkish": c.missing, "fsType": 0, "woff2": base64.StdEncoding.EncodeToString(c.woff2),
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// checkedWOFF2 stands for a checked font: core reads only its signature.
func checkedWOFF2(label string) []byte {
	return []byte("wOF2\x00\x01\x00\x00" + label)
}

// rawTTF stands for an upload: the media service tells it by its first
// bytes.
func rawTTF(label string) []byte {
	return []byte("\x00\x01\x00\x00" + label)
}

var (
	fontsArtlabLeader = authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/ARTLAB/LIDERLER"}}
	fontsArtlabMember = authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/ARTLAB"}}
	// A WEBLAB member with the template role: not a Leader.
	fontsWeblabManager = authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/WEBLAB"}, Roles: []string{"certificate:template:manage"}}
	// GECEKODU's members edit its Events (and its Event template), not its
	// templates.
	fontsGecekoduMember = authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ORGANIZASYON/GECEKODU"}}
	fontsYK             = authz.Principal{ID: uuid.NewString(), Groups: []string{"/YK"}}
	fontsDK             = authz.Principal{ID: uuid.NewString(), Groups: []string{"/DK"}}
)

type fontCertificates struct {
	service  certificate.Service
	store    *certificate.MemoryStore
	media    *media.MemoryStore
	mediaSvc media.Service
	storage  *media.PrivateStorage
	tools    *fontToolsContract
	render   *recRender
	gecekodu event.Event
}

func newFontCertificates(t *testing.T, tools *fontToolsContract) fontCertificates {
	t.Helper()
	ctx := context.Background()
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
	events := event.NewMemoryStore()
	gecekodu, err := events.Create(ctx, event.Event{Name: "Gece Kodu 2026", Location: "YTÜ", OwnerTeam: "GECEKODU"})
	if err != nil {
		t.Fatal(err)
	}
	store := certificate.NewMemoryStore()
	render := &recRender{}
	srv := tools.server(t)
	service := certificate.NewServiceWithOptions(store, ticket.NewMemoryStore(), events, user.NewMemoryStore(), az, render, &recMail{}, certificate.Options{
		PublicAPIOrigin: "https://api.example.test", VerifyOrigin: "https://skyl.app/c",
		Templates: store, Jobs: store, Artifacts: public, PrivateArtifacts: storage, Media: media.NewLinker(mediaStore),
		Assets: certificate.MediaAssets{Media: mediaStore, Blobs: public}, Uploads: mediaSvc,
		PDFTools: &certificate.PDFToolsClient{BaseURL: srv.URL, HTTP: srv.Client(), MaxBusyWait: time.Millisecond},
	})
	return fontCertificates{service: service, store: store, media: mediaStore, mediaSvc: mediaSvc, storage: storage, tools: tools, render: render, gecekodu: gecekodu}
}

// upload is p's certificate_font upload.
func (f fontCertificates) upload(t *testing.T, p authz.Principal, label string) uuid.UUID {
	t.Helper()
	created, err := f.mediaSvc.UploadForPurpose(context.Background(), p, media.PurposeCertificateFont, media.UploadedFile{
		Name: label + ".ttf", ContentType: "font/ttf", Data: rawTTF(label),
	})
	if err != nil {
		t.Fatalf("upload %s: %v", label, err)
	}
	return created.ID
}

// seed keeps a font of team as if uploaded and checked: its file is a
// private certificate_font Media.
func (f fontCertificates) seed(t *testing.T, team, family string, weight int, style string) certificate.Font {
	t.Helper()
	ctx := context.Background()
	key := "files/" + uuid.NewString()
	sealed, err := f.storage.Seal(ctx, media.PrivateObjectKey(key), checkedWOFF2(family))
	if err != nil {
		t.Fatal(err)
	}
	file, err := f.media.Create(ctx, media.Media{
		Name: family + ".woff2", Type: "font/woff2", Kind: media.KindFile, Key: sealed.Key, UploadedBy: uuid.New(),
		Purpose: media.PurposeCertificateFont, Visibility: media.VisibilityPrivate, Encryption: &sealed.Encryption,
	})
	if err != nil {
		t.Fatal(err)
	}
	font, err := f.store.CreateFont(ctx, certificate.Font{ID: uuid.New(), OwnerTeam: team, Family: family, Weight: weight, Style: style, MediaID: file.ID})
	if err != nil {
		t.Fatal(err)
	}
	return font
}

func fontLayout(fonts ...uuid.UUID) certificate.Layout {
	layout := certificate.Layout{
		Width: 1123, Height: 794, Orientation: "landscape",
		Elements: []certificate.Element{
			{ID: "name", Kind: "recipientName", X: 100, Y: 300, Width: 900, Height: 80, FontFamily: "Noto Sans", FontSize: 48, FontWeight: 700},
			{ID: "qr", Kind: "verificationQr", X: 950, Y: 620, Width: 130, Height: 130},
		},
	}
	for i, id := range fonts {
		id := id
		layout.Elements = append(layout.Elements, certificate.Element{
			ID: "text-" + string(rune('a'+i)), Kind: "staticText", Text: "Ağrı Dağı'nda gün ışığı", X: 100, Y: float64(100 + 60*i), Width: 600, Height: 50,
			FontFamily: "Brandon Grotesque", FontSize: 32, FontWeight: 400, FontMediaID: &id,
		})
	}
	return layout
}

func newFontTools() *fontToolsContract {
	return &fontToolsContract{family: "Brandon Grotesque", weight: 700, missing: "ğı", woff2: checkedWOFF2("brandon-700")}
}

// A Leader adds a font for the team: the upload goes to pdf-tools, the
// checked WOFF2 is kept as a new certificate_font Media (the upload is not
// the font), and the row says what pdf-tools said. The same face again is
// 409 font_exists; another weight is another font.
func TestCreateFontKeepsTheCheckedFile(t *testing.T) {
	ctx := context.Background()
	tools := newFontTools()
	f := newFontCertificates(t, tools)
	raw := f.upload(t, fontsArtlabLeader, "brandon")
	font, err := f.service.CreateFont(ctx, fontsArtlabLeader, certificate.FontInput{MediaID: raw, OwnerTeam: "artlab"})
	if err != nil {
		t.Fatal(err)
	}
	if font.OwnerTeam != "ARTLAB" || font.Family != "Brandon Grotesque" || font.Weight != 700 || font.Style != certificate.FontStyleNormal ||
		font.MissingTurkish != "ğı" || font.UploadedBy == nil || font.UploadedBy.String() != fontsArtlabLeader.ID || font.MediaID == raw {
		t.Fatalf("font: %+v", font)
	}
	if len(tools.calls) != 1 || !bytes.Equal(tools.calls[0], rawTTF("brandon")) {
		t.Fatalf("pdf-tools got %q", tools.calls)
	}
	stored, err := f.media.Get(ctx, font.MediaID)
	if err != nil || stored.Purpose != media.PurposeCertificateFont || stored.Type != "font/woff2" || stored.Visibility != media.VisibilityPrivate {
		t.Fatalf("checked file: %+v %v", stored, err)
	}
	file, err := f.service.FontFile(ctx, fontsArtlabLeader, font.ID)
	if err != nil || !bytes.Equal(file.Data, checkedWOFF2("brandon-700")) {
		t.Fatalf("file: %q %v", file.Data, err)
	}

	again := f.upload(t, fontsArtlabLeader, "brandon-again")
	tools.family = "brandon grotesque"
	_, err = f.service.CreateFont(ctx, fontsArtlabLeader, certificate.FontInput{MediaID: again, OwnerTeam: "ARTLAB"})
	var exists *certificate.FontExistsError
	if !errors.As(err, &exists) || exists.FontID != font.ID {
		t.Fatalf("same face: %v", err)
	}
	tools.weight = 400
	if _, err := f.service.CreateFont(ctx, fontsArtlabLeader, certificate.FontInput{MediaID: again, OwnerTeam: "ARTLAB"}); err != nil {
		t.Fatalf("another weight: %v", err)
	}
	// The same face in another team is that team's own.
	tools.weight = 700
	yk := f.upload(t, fontsYK, "club")
	if _, err := f.service.CreateFont(ctx, fontsYK, certificate.FontInput{MediaID: yk, OwnerTeam: ""}); err != nil {
		t.Fatalf("club's: %v", err)
	}
}

// Who adds and archives a team's fonts is who creates its templates: a
// Leader, a member with certificate:template:manage, a Privileged person
// (and only they for the club's). An Event's editor who may not create a
// template may not upload one (ticket 12's Event editors are not
// included), nor anyone else.
func TestFontUploadRules(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name       string
		p          authz.Principal
		team       string
		upload, ok bool
	}{
		{"ARTLAB Leader for ARTLAB", fontsArtlabLeader, "ARTLAB", true, true},
		{"ARTLAB Leader for the club", fontsArtlabLeader, "", true, false},
		{"ARTLAB Leader for WEBLAB", fontsArtlabLeader, "WEBLAB", true, false},
		{"WEBLAB template manager", fontsWeblabManager, "WEBLAB", true, true},
		{"GECEKODU Event editor", fontsGecekoduMember, "GECEKODU", false, false},
		{"ARTLAB member", fontsArtlabMember, "ARTLAB", false, false},
		{"YK for the club", fontsYK, "", true, true},
		{"YK for ARTLAB", fontsYK, "ARTLAB", true, true},
		{"DK for the club", fontsDK, "", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFontCertificates(t, newFontTools())
			_, err := f.mediaSvc.UploadForPurpose(ctx, c.p, media.PurposeCertificateFont, media.UploadedFile{Name: "a.ttf", Data: rawTTF("a")})
			if (err == nil) != c.upload {
				t.Fatalf("upload: %v", err)
			}
			// Their own upload, or a Leader's for those who may not upload.
			uploader := c.p
			if !c.upload {
				uploader = fontsYK
			}
			raw := f.upload(t, uploader, "b")
			_, err = f.service.CreateFont(ctx, c.p, certificate.FontInput{MediaID: raw, OwnerTeam: c.team})
			switch {
			case c.ok && err != nil:
				t.Fatalf("create: %v", err)
			case !c.ok && !errors.Is(err, certificate.ErrForbidden):
				t.Fatalf("create: %v, want forbidden", err)
			}
		})
	}
	if testing.Short() {
		return
	}
	cap := authz.NewAuthorizer(authz.DefaultPolicy())
	for p, want := range map[*authz.Principal]bool{&fontsArtlabLeader: true, &fontsGecekoduMember: false, &fontsYK: true, &fontsArtlabMember: false} {
		if got := cap.Capabilities(*p).Can["media.uploadCertificateFont"]; got != want {
			t.Errorf("%v media.uploadCertificateFont = %v", p.Groups, got)
		}
	}
}

// Only the uploader's own certificate_font upload is taken: another's, or a
// Media of another purpose, is 404 as if it were not there; pdf-tools is
// not called.
func TestCreateFontTakesOnlyTheCallersUpload(t *testing.T) {
	ctx := context.Background()
	tools := newFontTools()
	f := newFontCertificates(t, tools)
	other := f.upload(t, fontsYK, "yk")
	if _, err := f.service.CreateFont(ctx, fontsArtlabLeader, certificate.FontInput{MediaID: other, OwnerTeam: "ARTLAB"}); !errors.Is(err, certificate.ErrNotFound) {
		t.Fatalf("another's upload: %v", err)
	}
	background, err := f.mediaSvc.UploadForPurpose(ctx, fontsArtlabLeader, media.PurposeCertificateAsset, media.UploadedFile{Name: "a.pdf", Data: onePagePDF()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.CreateFont(ctx, fontsArtlabLeader, certificate.FontInput{MediaID: background.ID, OwnerTeam: "ARTLAB"}); !errors.Is(err, certificate.ErrNotFound) {
		t.Fatalf("a certificate asset: %v", err)
	}
	if _, err := f.service.CreateFont(ctx, fontsArtlabLeader, certificate.FontInput{MediaID: uuid.New(), OwnerTeam: "ARTLAB"}); !errors.Is(err, certificate.ErrNotFound) {
		t.Fatalf("no such Media: %v", err)
	}
	if len(tools.calls) != 0 {
		t.Fatalf("pdf-tools called %d times", len(tools.calls))
	}
}

// pdf-tools' refusals in core's codes (SPEC §9); pdf-tools down is 503
// pdf_tools_unavailable; an answer that is no WOFF2 is not stored.
func TestCreateFontRefusals(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		status    int
		code      string
		want      string
		available bool
	}{
		{422, "font_unreadable", certificate.CodeFontUnreadable, true},
		{422, "font_timeout", certificate.CodeFontUnreadable, true},
		{413, "font_too_large", certificate.CodeFontTooLarge, true},
		{422, "font_variable", certificate.CodeFontUnsupported, true},
		{422, "font_collection", certificate.CodeFontUnsupported, true},
		{422, "font_embedding_restricted", certificate.CodeFontEmbeddingRestricted, true},
		{500, "internal", certificate.CodePDFToolsUnavailable, false},
	} {
		t.Run(c.code, func(t *testing.T) {
			tools := newFontTools()
			tools.status, tools.code = c.status, c.code
			f := newFontCertificates(t, tools)
			raw := f.upload(t, fontsArtlabLeader, "x")
			_, err := f.service.CreateFont(ctx, fontsArtlabLeader, certificate.FontInput{MediaID: raw, OwnerTeam: "ARTLAB"})
			if c.available {
				refusedWith(t, err, c.want)
				return
			}
			var unavailable *certificate.UnavailableError
			if !errors.As(err, &unavailable) || unavailable.Code != c.want {
				t.Fatalf("got %v", err)
			}
		})
	}
	t.Run("no WOFF2", func(t *testing.T) {
		tools := newFontTools()
		tools.woff2 = rawTTF("not woff2")
		f := newFontCertificates(t, tools)
		raw := f.upload(t, fontsArtlabLeader, "x")
		_, err := f.service.CreateFont(ctx, fontsArtlabLeader, certificate.FontInput{MediaID: raw, OwnerTeam: "ARTLAB"})
		var unavailable *certificate.UnavailableError
		if !errors.As(err, &unavailable) {
			t.Fatalf("got %v", err)
		}
		if fonts, _ := f.store.ListFonts(ctx, []string{"ARTLAB"}); len(fonts) != 0 {
			t.Fatalf("kept %+v", fonts)
		}
	})
}

// Who uses a team's fonts: who reads or edits its templates, or edits its
// Events (an Event template is theirs). They list the team's and the
// club's and read their files; another team's are not theirs (the list is
// 403, a file 404). Archiving is the uploaders' rule.
func TestFontUseRules(t *testing.T) {
	ctx := context.Background()
	f := newFontCertificates(t, newFontTools())
	gece := f.seed(t, "GECEKODU", "Gece Sans", 400, certificate.FontStyleNormal)
	art := f.seed(t, "ARTLAB", "Art Serif", 400, certificate.FontStyleNormal)
	club := f.seed(t, "", "Kulüp", 700, certificate.FontStyleItalic)
	ids := func(fonts []certificate.Font) []uuid.UUID {
		out := make([]uuid.UUID, len(fonts))
		for i, font := range fonts {
			out[i] = font.ID
		}
		return out
	}
	same := func(a, b []uuid.UUID) bool {
		if len(a) != len(b) {
			return false
		}
		for i := range a {
			if a[i] != b[i] {
				return false
			}
		}
		return true
	}
	listed, err := f.service.ListFonts(ctx, fontsGecekoduMember, "gecekodu", nil)
	if err != nil || !same(ids(listed), []uuid.UUID{club.ID, gece.ID}) {
		t.Fatalf("GECEKODU member: %v %v", ids(listed), err)
	}
	if _, err := f.service.FontFile(ctx, fontsGecekoduMember, gece.ID); err != nil {
		t.Fatalf("GECEKODU member, own file: %v", err)
	}
	if _, err := f.service.FontFile(ctx, fontsGecekoduMember, club.ID); err != nil {
		t.Fatalf("GECEKODU member, club file: %v", err)
	}
	if _, err := f.service.FontFile(ctx, fontsGecekoduMember, art.ID); !errors.Is(err, certificate.ErrNotFound) {
		t.Fatalf("GECEKODU member, ARTLAB file: %v", err)
	}
	if _, err := f.service.ListFonts(ctx, fontsGecekoduMember, "ARTLAB", nil); !errors.Is(err, certificate.ErrForbidden) {
		t.Fatalf("GECEKODU member, ARTLAB list: %v", err)
	}
	if listed, err := f.service.ListFonts(ctx, fontsGecekoduMember, "", nil); err != nil || !same(ids(listed), []uuid.UUID{club.ID}) {
		t.Fatalf("GECEKODU member, the club's alone: %v %v", ids(listed), err)
	}
	if listed, err := f.service.ListFonts(ctx, fontsArtlabLeader, "ARTLAB", nil); err != nil || !same(ids(listed), []uuid.UUID{club.ID, art.ID}) {
		t.Fatalf("ARTLAB Leader: %v %v", ids(listed), err)
	}
	if _, err := f.service.ListFonts(ctx, fontsArtlabMember, "ARTLAB", nil); !errors.Is(err, certificate.ErrForbidden) {
		t.Fatalf("ARTLAB member without a role: %v", err)
	}
	if _, err := f.service.FontFile(ctx, fontsArtlabMember, art.ID); !errors.Is(err, certificate.ErrNotFound) {
		t.Fatalf("ARTLAB member without a role, file: %v", err)
	}

	// Archive: an Event editor may use but not archive; another team's is
	// not found; the Leader archives (twice is fine); the archived font
	// leaves the list, its file stays readable.
	if err := f.service.ArchiveFont(ctx, fontsGecekoduMember, gece.ID); !errors.Is(err, certificate.ErrForbidden) {
		t.Fatalf("Event editor archives: %v", err)
	}
	if err := f.service.ArchiveFont(ctx, fontsArtlabMember, gece.ID); !errors.Is(err, certificate.ErrNotFound) {
		t.Fatalf("someone else archives: %v", err)
	}
	if err := f.service.ArchiveFont(ctx, fontsArtlabLeader, art.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.service.ArchiveFont(ctx, fontsArtlabLeader, art.ID); err != nil {
		t.Fatalf("again: %v", err)
	}
	if listed, _ := f.service.ListFonts(ctx, fontsArtlabLeader, "ARTLAB", nil); !same(ids(listed), []uuid.UUID{club.ID}) {
		t.Fatalf("after archive: %v", ids(listed))
	}
	if _, err := f.service.FontFile(ctx, fontsArtlabLeader, art.ID); err != nil {
		t.Fatalf("archived file: %v", err)
	}
	if err := f.service.ArchiveFont(ctx, fontsArtlabLeader, club.ID); !errors.Is(err, certificate.ErrForbidden) {
		t.Fatalf("Leader archives the club's: %v", err)
	}
}

// A text element may use a current font of its template's team or the
// club's; another team's is layout_font_out_of_scope, and so is an
// archived one, newly linked. A draft that has a font keeps it after it
// is archived. An Event template takes its Event's team: its Event editor
// (no template role) sets text in the team's font.
func TestLayoutFontScope(t *testing.T) {
	ctx := context.Background()
	f := newFontCertificates(t, newFontTools())
	art := f.seed(t, "ARTLAB", "Art Serif", 400, certificate.FontStyleNormal)
	gece := f.seed(t, "GECEKODU", "Gece Sans", 400, certificate.FontStyleNormal)
	club := f.seed(t, "", "Kulüp", 400, certificate.FontStyleNormal)
	spare := f.seed(t, "ARTLAB", "Art Serif", 700, certificate.FontStyleNormal)

	draft := certificate.TemplateDraft{Name: "ARTLAB", OwnerTeam: "ARTLAB", SourceKind: "sky", Layout: fontLayout(art.MediaID, club.MediaID)}
	item, err := f.service.CreateTemplate(ctx, fontsArtlabLeader, draft)
	if err != nil {
		t.Fatalf("own and club fonts: %v", err)
	}
	other := draft
	other.Layout = fontLayout(gece.MediaID)
	_, err = f.service.CreateTemplate(ctx, fontsArtlabLeader, other)
	refusedWith(t, err, certificate.CodeLayoutFontOutOfScope)

	if err := f.service.ArchiveFont(ctx, fontsArtlabLeader, art.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.service.ArchiveFont(ctx, fontsArtlabLeader, spare.ID); err != nil {
		t.Fatal(err)
	}
	// The archived font stays in the draft that has it…
	draft.Name = "ARTLAB 2"
	if _, err := f.service.UpdateTemplate(ctx, fontsArtlabLeader, item.ID, draft); err != nil {
		t.Fatalf("keep an archived font: %v", err)
	}
	// …and no new link takes one.
	draft.Layout = fontLayout(art.MediaID, club.MediaID, spare.MediaID)
	_, err = f.service.UpdateTemplate(ctx, fontsArtlabLeader, item.ID, draft)
	refusedWith(t, err, certificate.CodeLayoutFontOutOfScope)
	// A Media that is no certificate font is refused as a font.
	asset, err := f.mediaSvc.UploadForPurpose(ctx, fontsArtlabLeader, media.PurposeCertificateAsset, media.UploadedFile{Name: "a.pdf", Data: onePagePDF()})
	if err != nil {
		t.Fatal(err)
	}
	draft.Layout = fontLayout(art.MediaID, club.MediaID, asset.ID)
	_, err = f.service.UpdateTemplate(ctx, fontsArtlabLeader, item.ID, draft)
	refusedWith(t, err, certificate.CodeLayoutFontOutOfScope)
	// A font on an element that draws no text is a style error.
	bad := fontLayout()
	bad.Elements[1].FontMediaID = &club.MediaID
	draft.Layout = bad
	_, err = f.service.UpdateTemplate(ctx, fontsArtlabLeader, item.ID, draft)
	refusedWith(t, err, certificate.CodeLayoutElementStyle)

	// An Event template: GECEKODU's Event editor uses GECEKODU's font, not
	// ARTLAB's.
	own, err := f.service.CreateEventTemplate(ctx, fontsGecekoduMember, f.gecekodu.ID, certificate.EventTemplateInput{From: certificate.EventTemplateFromBlank})
	if err != nil {
		t.Fatal(err)
	}
	eventDraft := certificate.TemplateDraft{Name: own.Name, SourceKind: own.SourceKind, Layout: fontLayout(gece.MediaID, club.MediaID)}
	if _, err := f.service.UpdateTemplate(ctx, fontsGecekoduMember, own.ID, eventDraft); err != nil {
		t.Fatalf("Event editor, team font: %v", err)
	}
	eventDraft.Layout = fontLayout(gece.MediaID, club.MediaID, spare.MediaID)
	_, err = f.service.UpdateTemplate(ctx, fontsGecekoduMember, own.ID, eventDraft)
	refusedWith(t, err, certificate.CodeLayoutFontOutOfScope)
}

// The page embeds the font a text element uses, at the element's weight
// and style, and sets the element in it before its family's stack; a font
// no element uses is not on the page. A published version copies the
// font's file, so it prints the same after the font is archived.
func TestLayoutHTMLEmbedsCertificateFonts(t *testing.T) {
	ctx := context.Background()
	f := newFontCertificates(t, newFontTools())
	used := f.seed(t, "ARTLAB", "Art Serif", 400, certificate.FontStyleNormal)
	unused := f.seed(t, "ARTLAB", "Art Sans", 400, certificate.FontStyleNormal)
	// Two elements in the font, at two weights: one face.
	layout := fontLayout(used.MediaID, used.MediaID)
	layout.Elements[2].FontStyle = certificate.FontStyleItalic
	layout.Elements[3].FontStyle = certificate.FontStyleItalic
	layout.Elements[3].FontWeight = 700
	item, err := f.service.CreateTemplate(ctx, fontsArtlabLeader, certificate.TemplateDraft{Name: "ARTLAB", OwnerTeam: "ARTLAB", SourceKind: "sky", Layout: layout})
	if err != nil {
		t.Fatal(err)
	}
	family := "cf-" + used.MediaID.String()
	face := `@font-face{font-family:'` + family + `';font-style:italic;font-weight:1 1000;src:url(data:font/woff2;base64,` +
		base64.StdEncoding.EncodeToString(checkedWOFF2("Art Serif")) + `) format('woff2')}`
	check := func(label string) {
		t.Helper()
		html := f.render.html
		if !strings.Contains(html, face) {
			t.Fatalf("%s: no face in\n%s", label, html)
		}
		if strings.Count(html, "@font-face{font-family:'"+family) != 1 || strings.Contains(html, unused.MediaID.String()) {
			t.Fatalf("%s: faces", label)
		}
		if !strings.Contains(html, `font-family:'`+family+`','Liberation Sans',Arial,sans-serif;`) {
			t.Fatalf("%s: element stack", label)
		}
		if !strings.Contains(html, "window.__certFitDone=true") {
			t.Fatalf("%s: does not wait for the font", label)
		}
	}
	if _, err := f.service.PreviewTemplate(ctx, fontsArtlabLeader, item.ID, certificate.PreviewData{RecipientName: "Ayşe Yıldız"}); err != nil {
		t.Fatal(err)
	}
	check("draft")
	version, err := f.service.PublishTemplate(ctx, fontsArtlabLeader, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	check("publish")
	stored, err := f.store.GetVersion(ctx, version.ID)
	if err != nil {
		t.Fatal(err)
	}
	ref, ok := stored.AssetManifest[used.MediaID.String()]
	if !ok || ref.ContentType != "font/woff2" || ref.Encryption == nil {
		t.Fatalf("manifest: %+v", stored.AssetManifest)
	}
	// Archived, the font still prints in the draft that has it: a new
	// version renders it from its own copy.
	if err := f.service.ArchiveFont(ctx, fontsArtlabLeader, used.ID); err != nil {
		t.Fatal(err)
	}
	f.render.html = ""
	if _, err := f.service.PublishTemplate(ctx, fontsArtlabLeader, item.ID); err != nil {
		t.Fatalf("publish after archive: %v", err)
	}
	check("publish after archive")
}

// A text conversion matches the PDF's fonts against the template's team's
// and the club's fonts first: Brandon Grotesque uploaded by ARTLAB sets the
// placeholder name in it (fontMediaId), not in a stand-in; without the
// template, ARTLAB's font is not offered.
func TestConversionUsesTheTeamsFonts(t *testing.T) {
	ctx := context.Background()
	text := &textToolsContract{inspect: certificateInspect(), stripped: []byte("%PDF-1.7 stripped")}
	f := newFontCertificates(t, &fontToolsContract{text: text})
	brandon := f.seed(t, "ARTLAB", "Brandon Grotesque", 400, certificate.FontStyleNormal)
	original, err := f.mediaSvc.UploadForPurpose(ctx, fontsArtlabLeader, media.PurposeCertificateAsset, media.UploadedFile{Name: "canva.pdf", Data: onePagePDF()})
	if err != nil {
		t.Fatal(err)
	}
	item, err := f.service.CreateTemplate(ctx, fontsArtlabLeader, certificate.TemplateDraft{Name: "ARTLAB", OwnerTeam: "ARTLAB", SourceKind: "sky", Layout: fontLayout()})
	if err != nil {
		t.Fatal(err)
	}
	listed, err := f.service.AssetText(ctx, fontsArtlabLeader, original.ID, &item.ID)
	if err != nil {
		t.Fatal(err)
	}
	var name certificate.AssetTextSpan
	for _, span := range listed.Spans {
		if span.ID == "5" {
			name = span
		}
	}
	if name.Font != "Brandon Grotesque" || name.FontSubstituted || name.FontMediaID == nil || *name.FontMediaID != brandon.MediaID {
		t.Fatalf("listed: %+v", name)
	}
	req := conversionRequest("5")
	req.TemplateID = &item.ID
	converted, err := f.service.ConvertAssetText(ctx, fontsArtlabLeader, original.ID, req)
	if err != nil {
		t.Fatal(err)
	}
	if len(converted.Elements) != 1 || converted.Elements[0].FontMediaID == nil || *converted.Elements[0].FontMediaID != brandon.MediaID {
		t.Fatalf("elements: %+v", converted.Elements)
	}
	for _, w := range converted.Warnings {
		if w.Kind == certificate.WarningFontSubstituted {
			t.Fatalf("warned %+v", w)
		}
	}
	// The draft takes the element: the font is ARTLAB's.
	layout := fontLayout()
	layout.Elements = append(layout.Elements, converted.Elements...)
	if _, err := f.service.UpdateTemplate(ctx, fontsArtlabLeader, item.ID, certificate.TemplateDraft{Name: "ARTLAB", OwnerTeam: "ARTLAB", SourceKind: "sky", Layout: layout}); err != nil {
		t.Fatalf("save: %v", err)
	}

	listed, err = f.service.AssetText(ctx, fontsArtlabLeader, original.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, span := range listed.Spans {
		if span.FontMediaID != nil {
			t.Fatalf("without a template: %+v", span)
		}
	}
	// Someone who may not edit the template gets nothing of it.
	if _, err := f.service.AssetText(ctx, fontsGecekoduMember, original.ID, &item.ID); err == nil {
		t.Fatal("another team's template")
	}
}

// An Event template copies a published template whose font was archived
// since: it is created (201), the font kept as in the source.
func TestEventTemplateKeepsTheSourcesArchivedFont(t *testing.T) {
	ctx := context.Background()
	f := newFontCertificates(t, newFontTools())
	gece := f.seed(t, "GECEKODU", "Gece Sans", 400, certificate.FontStyleNormal)
	leader := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ORGANIZASYON/GECEKODU/LIDERLER"}}
	item, err := f.service.CreateTemplate(ctx, leader, certificate.TemplateDraft{Name: "GECEKODU", OwnerTeam: "GECEKODU", SourceKind: "sky", Layout: fontLayout(gece.MediaID)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.PublishTemplate(ctx, leader, item.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.service.ArchiveFont(ctx, leader, gece.ID); err != nil {
		t.Fatal(err)
	}
	own, err := f.service.CreateEventTemplate(ctx, fontsGecekoduMember, f.gecekodu.ID, certificate.EventTemplateInput{From: item.ID.String()})
	if err != nil {
		t.Fatalf("Event template from the source: %v", err)
	}
	if got := own.DraftLayout.Elements[2].FontMediaID; got == nil || *got != gece.MediaID {
		t.Fatalf("font: %+v", own.DraftLayout.Elements[2])
	}
	// Its draft saves as it is; its editor's list marks the archived font.
	if _, err := f.service.UpdateTemplate(ctx, fontsGecekoduMember, own.ID, certificate.TemplateDraft{Name: own.Name, SourceKind: own.SourceKind, Layout: own.DraftLayout}); err != nil {
		t.Fatalf("save the copy: %v", err)
	}
	listed, err := f.service.ListFonts(ctx, fontsGecekoduMember, "", &own.ID)
	if err != nil || len(listed) != 1 || listed[0].ID != gece.ID || !listed[0].DraftOnly {
		t.Fatalf("list: %+v %v", listed, err)
	}
}

// A font's file, or a font upload, is no background and no image: refused
// when the draft is saved (asset_type_unsupported).
func TestFontMediaIsNoBackgroundNorImage(t *testing.T) {
	ctx := context.Background()
	f := newFontCertificates(t, newFontTools())
	checked := f.seed(t, "ARTLAB", "Art Serif", 400, certificate.FontStyleNormal)
	raw := f.upload(t, fontsArtlabLeader, "raw")
	for name, id := range map[string]uuid.UUID{"checked file": checked.MediaID, "upload": raw} {
		id := id
		background := fontLayout()
		background.BackgroundMediaID = &id
		_, err := f.service.CreateTemplate(ctx, fontsArtlabLeader, certificate.TemplateDraft{Name: "A", OwnerTeam: "ARTLAB", SourceKind: "sky", Layout: background})
		refusedWith(t, err, certificate.CodeAssetTypeUnsupported)
		image := fontLayout()
		image.Elements = append(image.Elements, certificate.Element{ID: "logo", Kind: "image", MediaID: &id, X: 10, Y: 10, Width: 100, Height: 100})
		_, err = f.service.CreateTemplate(ctx, fontsArtlabLeader, certificate.TemplateDraft{Name: "A", OwnerTeam: "ARTLAB", SourceKind: "sky", Layout: image})
		if err == nil {
			t.Fatalf("%s as an image", name)
		}
		refusedWith(t, err, certificate.CodeAssetTypeUnsupported)
	}
}

// A template's editor lists its team's and the club's fonts, and those its
// draft keeps (an archived one, another team's) marked draftOnly; someone
// who may not edit it gets nothing. Who may read a template that uses a
// font reads its file, whatever team the font is.
func TestTemplateFontsList(t *testing.T) {
	ctx := context.Background()
	f := newFontCertificates(t, newFontTools())
	art := f.seed(t, "ARTLAB", "Art Serif", 400, certificate.FontStyleNormal)
	gone := f.seed(t, "ARTLAB", "Art Sans", 400, certificate.FontStyleNormal)
	club := f.seed(t, "", "Kulüp", 400, certificate.FontStyleNormal)
	gece := f.seed(t, "GECEKODU", "Gece Sans", 400, certificate.FontStyleNormal)
	item, err := f.service.CreateTemplate(ctx, fontsArtlabLeader, certificate.TemplateDraft{Name: "ARTLAB", OwnerTeam: "ARTLAB", SourceKind: "sky", Layout: fontLayout(gone.MediaID)})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.ArchiveFont(ctx, fontsArtlabLeader, gone.ID); err != nil {
		t.Fatal(err)
	}
	listed, err := f.service.ListFonts(ctx, fontsArtlabLeader, "", &item.ID)
	if err != nil {
		t.Fatal(err)
	}
	got := map[uuid.UUID]bool{}
	for _, font := range listed {
		got[font.ID] = font.DraftOnly
	}
	if len(got) != 3 || got[art.ID] || got[club.ID] || !got[gone.ID] {
		t.Fatalf("list: %+v", listed)
	}
	if _, ok := got[gece.ID]; ok {
		t.Fatal("another team's font offered")
	}
	if _, err := f.service.ListFonts(ctx, fontsGecekoduMember, "", &item.ID); err == nil {
		t.Fatal("someone who may not edit the template")
	}
	// GECEKODU reads no ARTLAB font, but a GECEKODU template that uses one
	// (a copy whose team changed) lets its readers load it.
	if _, err := f.service.FontFile(ctx, fontsGecekoduMember, art.ID); !errors.Is(err, certificate.ErrNotFound) {
		t.Fatalf("before: %v", err)
	}
	if _, err := f.store.CreateTemplate(ctx, certificate.Template{ID: uuid.New(), Name: "GECEKODU", OwnerTeam: "GECEKODU", SourceKind: "sky", DraftLayout: fontLayout(art.MediaID)}); err != nil {
		t.Fatal(err)
	}
	leader := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ORGANIZASYON/GECEKODU/LIDERLER"}}
	if _, err := f.service.FontFile(ctx, leader, art.ID); err != nil {
		t.Fatalf("a template that uses it: %v", err)
	}
}
