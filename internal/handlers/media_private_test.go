package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/config"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/clientip"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/transit"
	"github.com/skylab-kulubu/core-backend/internal/transit/transittest"
)

// privateMediaHTTP is the media routes with private Media on.
type privateMediaHTTP struct {
	svc     media.Service
	store   *media.MemoryStore
	private *media.MemoryBlob
	bao     *transittest.Server
	logs    *logRecorder
}

// logRecorder keeps what the handler logs.
type logRecorder struct {
	mu    sync.Mutex
	lines []string
}

func (l *logRecorder) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logRecorder) all() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

// unscannedCatalogue is the reviewed catalogue with answer_file's malware
// scan lifted: no scanner exists until ticket 12, so the reviewed
// answer_file cannot be uploaded at all.
func unscannedCatalogue(t testing.TB) media.Catalogue {
	t.Helper()
	var file map[string]any
	if err := json.Unmarshal(config.MediaPurposes, &file); err != nil {
		t.Fatal(err)
	}
	file["purposes"].(map[string]any)["answer_file"].(map[string]any)["scan"] = false
	raw, err := json.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}
	catalogue, err := media.ParseCatalogue(raw)
	if err != nil {
		t.Fatal(err)
	}
	return catalogue
}

func newPrivateMediaHTTP(t *testing.T) privateMediaHTTP {
	t.Helper()
	return newPrivateMediaHTTPWith(t, nil)
}

// newPrivateMediaHTTPWith writes the access log through accessLog when it
// is set.
func newPrivateMediaHTTPWith(t *testing.T, accessLog func(*media.MemoryStore) media.AccessLog) privateMediaHTTP {
	t.Helper()
	bao := transittest.NewServer(t)
	store := media.NewMemoryStore()
	private := media.NewMemoryBlob()
	var log media.AccessLog = store
	if accessLog != nil {
		log = accessLog(store)
	}
	svc := media.NewServiceWithOptions(store, media.NewMemoryBlob(), authz.NewAuthorizer(authz.DefaultPolicy()), "https://cdn.example.test",
		media.ServiceOptions{
			ServiceProducts: []authz.Product{authz.ProductForms},
			Catalogue:       unscannedCatalogue(t),
			Private: &media.PrivateMedia{
				Storage: media.NewPrivateStorage(private, transit.New(bao.Config())),
				LinkKey: bytes.Repeat([]byte{3}, 32), LinkOrigin: "https://api.example.test",
				AccessLog: log, Now: bao.Clock.Now,
			},
		})
	return privateMediaHTTP{svc: svc, store: store, private: private, bao: bao, logs: &logRecorder{}}
}

// app serves the media routes to ident; the content route needs no one.
func (h privateMediaHTTP) app(t *testing.T, ident authn.Identity) *fiber.App {
	t.Helper()
	app := mediaServiceApp(t, ident, h.svc)
	handler := NewMediaHandler(h.svc).TrustProxies(clientip.Default())
	handler.logf = h.logs.logf
	app.Post("/v1/media/:id/links", handler.IssueReadLink)
	app.Get("/v1/media/:id/content", handler.Content)
	return app
}

var (
	respondentHTTP = authn.Identity{ID: uuid.MustParse("90909090-9090-9090-9090-909090909090")}
	formsHTTP      = authn.Identity{ID: uuid.New(), Product: authz.ProductForms, Roles: []string{"media:attach"}}
	reviewerHTTP   = uuid.MustParse("91919191-9191-9191-9191-919191919191")
)

func (h privateMediaHTTP) uploadAnswerFile(t *testing.T, name string) media.Media {
	t.Helper()
	resp := postMedia(t, h.app(t, respondentHTTP), "answer_file", name, []byte("%PDF-1.7\n%%EOF\n"))
	if resp.status != fiber.StatusCreated {
		t.Fatalf("upload: status %d body %v", resp.status, resp.body)
	}
	id := uuid.MustParse(resp.body["id"].(string))
	if resp.body["url"] != "" || resp.body["visibility"] != "private" {
		t.Fatalf("upload answered %v", resp.body)
	}
	return media.Media{ID: id}
}

func do(t *testing.T, app *fiber.App, req *http.Request) (*http.Response, []byte) {
	t.Helper()
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, raw
}

func (h privateMediaHTTP) issueLink(t *testing.T, ident authn.Identity, id uuid.UUID) (*http.Response, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(fiber.MethodPost, "/v1/media/"+id.String()+"/links", strings.NewReader(`{"onBehalfOf":"`+reviewerHTTP.String()+`"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, raw := do(t, h.app(t, ident), req)
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("status %d body %s", resp.StatusCode, raw)
	}
	return resp, body
}

// contentPath is the path and query of a read link.
func contentPath(t *testing.T, link string) string {
	t.Helper()
	parsed, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	return parsed.RequestURI()
}

func requireProblemCode(t *testing.T, resp *http.Response, raw []byte, status int, code string) {
	t.Helper()
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	if resp.StatusCode != status || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/problem+json") || body["code"] != code {
		t.Fatalf("status %d body %s; want %d %s", resp.StatusCode, raw, status, code)
	}
}

func TestPrivateMediaReadLinkOpensAnAttachmentDownloadHTTP(t *testing.T) {
	t.Parallel()
	h := newPrivateMediaHTTP(t)
	file := h.uploadAnswerFile(t, "Özge CV.pdf")

	resp, link := h.issueLink(t, formsHTTP, file.ID)
	if resp.StatusCode != fiber.StatusCreated || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("link: status %d cache %q body %v", resp.StatusCode, resp.Header.Get("Cache-Control"), link)
	}
	expiresAt, err := time.Parse(time.RFC3339, link["expiresAt"].(string))
	if err != nil || !expiresAt.Equal(h.bao.Clock.Now().Add(5*time.Minute)) {
		t.Fatalf("expiresAt %v", link["expiresAt"])
	}

	req := httptest.NewRequest(fiber.MethodGet, contentPath(t, link["url"].(string)), nil)
	resp, raw := do(t, h.app(t, authn.Identity{}), req)
	if resp.StatusCode != fiber.StatusOK || !bytes.Equal(raw, []byte("%PDF-1.7\n%%EOF\n")) {
		t.Fatalf("content: status %d body %q", resp.StatusCode, raw)
	}
	for header, want := range map[string]string{
		"Content-Type":           "application/pdf",
		"Content-Disposition":    "attachment; filename*=UTF-8''%C3%96zge%20CV.pdf",
		"X-Content-Type-Options": "nosniff",
		"Cache-Control":          "private, no-store",
		"Referrer-Policy":        "no-referrer",
	} {
		if got := resp.Header.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
}

func TestPrivateMediaReadLinkTokenMustBeValidHTTP(t *testing.T) {
	t.Parallel()
	h := newPrivateMediaHTTP(t)
	file := h.uploadAnswerFile(t, "cv.pdf")
	_, link := h.issueLink(t, formsHTTP, file.ID)
	path := contentPath(t, link["url"].(string))
	anonymous := h.app(t, authn.Identity{})

	resp, raw := do(t, anonymous, httptest.NewRequest(fiber.MethodGet, strings.Replace(path, "token=", "token=x", 1), nil))
	requireProblemCode(t, resp, raw, fiber.StatusForbidden, "media_link_invalid")
	if bytes.Contains(raw, []byte("token")) {
		t.Fatalf("the problem repeats the token: %s", raw)
	}

	h.bao.Clock.Advance(5 * time.Minute)
	resp, raw = do(t, anonymous, httptest.NewRequest(fiber.MethodGet, path, nil))
	requireProblemCode(t, resp, raw, fiber.StatusForbidden, "media_link_expired")
}

func TestPrivateMediaReadLinkIsRefusedToPeopleHTTP(t *testing.T) {
	t.Parallel()
	h := newPrivateMediaHTTP(t)
	file := h.uploadAnswerFile(t, "cv.pdf")

	resp, body := h.issueLink(t, respondentHTTP, file.ID)
	if resp.StatusCode != fiber.StatusForbidden || body["code"] != "media_link_forbidden" {
		t.Fatalf("status %d body %v", resp.StatusCode, body)
	}
}

func TestPrivateMediaMetadataIsNotFoundForAnonymousAndMembersHTTP(t *testing.T) {
	t.Parallel()
	h := newPrivateMediaHTTP(t)
	file := h.uploadAnswerFile(t, "cv.pdf")

	for name, ident := range map[string]authn.Identity{"anonymous": {}, "the uploader": respondentHTTP} {
		resp, raw := do(t, h.app(t, ident), httptest.NewRequest(fiber.MethodGet, "/v1/media/"+file.ID.String(), nil))
		if resp.StatusCode != fiber.StatusNotFound {
			t.Errorf("%s: status %d body %s", name, resp.StatusCode, raw)
		}
	}
	resp, raw := do(t, h.app(t, formsHTTP), httptest.NewRequest(fiber.MethodGet, "/v1/media/"+file.ID.String(), nil))
	if resp.StatusCode != fiber.StatusOK || bytes.Contains(raw, []byte("private/")) {
		t.Fatalf("Skyforms: status %d body %s", resp.StatusCode, raw)
	}
}

func TestOpenBaoDownIsARetryable503ForPrivateMediaOnlyHTTP(t *testing.T) {
	t.Parallel()
	h := newPrivateMediaHTTP(t)
	file := h.uploadAnswerFile(t, "cv.pdf")
	_, link := h.issueLink(t, formsHTTP, file.ID)
	h.bao.Seal()

	resp, raw := do(t, h.app(t, authn.Identity{}), httptest.NewRequest(fiber.MethodGet, contentPath(t, link["url"].(string)), nil))
	requireProblemCode(t, resp, raw, fiber.StatusServiceUnavailable, "private_media_unavailable")
	if resp.Header.Get("Retry-After") == "" {
		t.Fatal("no Retry-After")
	}

	upload := postMedia(t, h.app(t, respondentHTTP), "answer_file", "cv.pdf", []byte("%PDF-1.7\n%%EOF\n"))
	if upload.status != fiber.StatusServiceUnavailable || upload.body["code"] != "private_media_unavailable" {
		t.Fatalf("private upload: status %d body %v", upload.status, upload.body)
	}
	picture := postMedia(t, h.app(t, respondentHTTP), "profile_picture", "me.png", pngDotHTTP())
	if picture.status != fiber.StatusCreated {
		t.Fatalf("public upload: status %d body %v", picture.status, picture.body)
	}
}

func TestReadLinksAreRefusedWhilePrivateMediaIsOffHTTP(t *testing.T) {
	t.Parallel()
	svc := media.NewService(media.NewMemoryStore(), media.NewMemoryBlob(), authz.NewAuthorizer(authz.DefaultPolicy()), "")
	app := mediaServiceApp(t, formsHTTP, svc)
	handler := NewMediaHandler(svc)
	app.Post("/v1/media/:id/links", handler.IssueReadLink)

	req := httptest.NewRequest(fiber.MethodPost, "/v1/media/"+uuid.NewString()+"/links", strings.NewReader(`{"onBehalfOf":"`+reviewerHTTP.String()+`"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, raw := do(t, app, req)
	requireProblemCode(t, resp, raw, fiber.StatusUnprocessableEntity, "private_media_disabled")
}

func TestChangedPrivateObjectIsNeverServedHTTP(t *testing.T) {
	t.Parallel()
	h := newPrivateMediaHTTP(t)
	file := h.uploadAnswerFile(t, "cv.pdf")
	stored, err := h.store.Get(context.Background(), file.ID)
	if err != nil {
		t.Fatal(err)
	}
	sealed, _ := h.private.Get(stored.Key)
	changed := bytes.Clone(sealed)
	changed[len(changed)-1] ^= 0x01
	if err := h.private.Put(context.Background(), stored.Key, changed, media.BlobMetadata{}); err != nil {
		t.Fatal(err)
	}
	_, link := h.issueLink(t, formsHTTP, file.ID)

	resp, raw := do(t, h.app(t, authn.Identity{}), httptest.NewRequest(fiber.MethodGet, contentPath(t, link["url"].(string)), nil))
	requireProblemCode(t, resp, raw, fiber.StatusInternalServerError, "private_media_integrity")
	if bytes.Contains(raw, []byte("%PDF")) {
		t.Fatal("served plaintext of a changed object")
	}
}

// A segment that fails its check after the answer has started cannot turn
// into an error status: the download ends short, and the log names the Media
// and the segment.
func TestChangedSegmentMidStreamIsLoggedHTTP(t *testing.T) {
	t.Parallel()
	h := newPrivateMediaHTTP(t)
	file := append([]byte("%PDF-1.7\n"), bytes.Repeat([]byte("0"), 200<<10)...)
	resp := postMedia(t, h.app(t, respondentHTTP), "answer_file", "portfolio.pdf", file)
	if resp.status != fiber.StatusCreated {
		t.Fatalf("upload: status %d body %v", resp.status, resp.body)
	}
	id := uuid.MustParse(resp.body["id"].(string))
	stored, err := h.store.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	sealed, _ := h.private.Get(stored.Key)
	changed := bytes.Clone(sealed)
	changed[16+(64<<10)+16+100] ^= 0x01 // inside the second segment
	if err := h.private.Put(context.Background(), stored.Key, changed, media.BlobMetadata{}); err != nil {
		t.Fatal(err)
	}
	_, link := h.issueLink(t, formsHTTP, id)

	req := httptest.NewRequest(fiber.MethodGet, contentPath(t, link["url"].(string)), nil)
	got, err := h.app(t, authn.Identity{}).Test(req)
	if err == nil {
		body, _ := io.ReadAll(got.Body)
		got.Body.Close()
		if len(body) >= len(file) {
			t.Fatal("the whole file was served")
		}
	}
	logged := h.logs.all()
	if !strings.Contains(logged, id.String()) || !strings.Contains(logged, "segment 1") {
		t.Fatalf("logged %q", logged)
	}
}

// An admin gets a link to core's own private Media (a certificate asset)
// for themselves.
func TestAdminReadLinkToACertificateAssetHTTP(t *testing.T) {
	t.Parallel()
	h := newPrivateMediaHTTP(t)
	admin := authn.Identity{ID: uuid.New(), Groups: []string{"/UYELER/YK"}}
	upload := postMedia(t, h.app(t, admin), "certificate_asset", "background.png", pngDotHTTP())
	if upload.status != fiber.StatusCreated {
		t.Fatalf("upload: status %d body %v", upload.status, upload.body)
	}
	id := upload.body["id"].(string)

	req := httptest.NewRequest(fiber.MethodPost, "/v1/media/"+id+"/links", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	resp, raw := do(t, h.app(t, admin), req)
	var link map[string]any
	if err := json.Unmarshal(raw, &link); err != nil || resp.StatusCode != fiber.StatusCreated {
		t.Fatalf("link: status %d body %s", resp.StatusCode, raw)
	}
	resp, raw = do(t, h.app(t, authn.Identity{}), httptest.NewRequest(fiber.MethodGet, contentPath(t, link["url"].(string)), nil))
	if resp.StatusCode != fiber.StatusOK || !bytes.Equal(raw, pngDotHTTP()) {
		t.Fatalf("content: status %d", resp.StatusCode)
	}
}

// inactiveSubjects is an access log that refuses every link as naming an
// account that is not active, as PostgreSQL does for one being erased.
type inactiveSubjects struct{ *media.MemoryStore }

func (inactiveSubjects) RecordReadLink(context.Context, media.ReadLinkRecord) error {
	return media.ErrLinkSubjectInactive
}

func TestReadLinkForAnInactiveAccountHTTP(t *testing.T) {
	t.Parallel()
	h := newPrivateMediaHTTPWith(t, func(store *media.MemoryStore) media.AccessLog { return inactiveSubjects{store} })
	file := h.uploadAnswerFile(t, "cv.pdf")

	resp, body := h.issueLink(t, formsHTTP, file.ID)
	if resp.StatusCode != fiber.StatusUnprocessableEntity || body["code"] != "media_link_subject_inactive" {
		t.Fatalf("status %d body %v", resp.StatusCode, body)
	}
}

// With no malware scanner, an Answer file cannot be uploaded.
func TestAnswerFileNeedsAScannerHTTP(t *testing.T) {
	t.Parallel()
	bao := transittest.NewServer(t)
	store := media.NewMemoryStore()
	svc := media.NewServiceWithOptions(store, media.NewMemoryBlob(), authz.NewAuthorizer(authz.DefaultPolicy()), "",
		media.ServiceOptions{
			ServiceProducts: []authz.Product{authz.ProductForms},
			Private:         &media.PrivateMedia{Storage: media.NewPrivateStorage(media.NewMemoryBlob(), transit.New(bao.Config())), AccessLog: store},
		})
	resp := postMedia(t, mediaServiceApp(t, respondentHTTP, svc), "answer_file", "cv.pdf", []byte("%PDF-1.7\n"))
	requireProblem(t, resp, fiber.StatusUnprocessableEntity, "purpose_not_available")
	if !strings.Contains(resp.body["detail"].(string), "scan") {
		t.Fatalf("detail %v", resp.body["detail"])
	}
}
