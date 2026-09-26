package httpx_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/httpx"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/testauth"
	"github.com/skylab-kulubu/core-backend/internal/user"
	"github.com/valyala/fasthttp/fasthttputil"
)

// manualClock is the upload limiter's clock, moved by hand.
type manualClock struct {
	mu  sync.Mutex
	now time.Time
}

func newManualClock() *manualClock {
	return &manualClock{now: time.Date(2026, 9, 26, 9, 0, 0, 0, time.UTC)}
}

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *manualClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func limitedUploadApp(keys *testauth.Bundle, limiter *media.UploadLimiter) *fiber.App {
	return limitedUploadAppWithBlobs(keys, limiter, media.NewMemoryBlob())
}

func limitedUploadAppWithBlobs(keys *testauth.Bundle, limiter *media.UploadLimiter, blobs media.BlobStore) *fiber.App {
	az := authz.NewAuthorizer(authz.DefaultPolicy())
	return httpx.New(httpx.Deps{
		Users:              user.NewService(user.NewMemoryStore()),
		Media:              media.NewService(media.NewMemoryStore(), blobs, az, ""),
		ParseToken:         keys.Parse(),
		MediaUploadLimiter: limiter,
		TrustedProxies:     testTrustedProxies(),
	})
}

// newPersonBearer is the bearer token of a new signed-in person.
func newPersonBearer(t *testing.T, keys *testauth.Bundle) string {
	t.Helper()
	return keys.Token(t, jwt.MapClaims{
		"sub": uuid.NewString(), "email": uuid.NewString() + "@example.com", "given_name": "Ada", "family_name": "Lovelace",
	})
}

func pngPicture(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// pdfFormBytes is the length of pdfForm's body for a PDF of size bytes.
func pdfFormBytes(t *testing.T, size int) int64 {
	t.Helper()
	body, _ := pdfForm(t, size)
	return int64(body.Len())
}

type limitedUploadResponse struct {
	status     int
	retryAfter string
	body       map[string]any
}

// postLimitedUpload posts a multipart form to path; an empty bearer is
// anonymous.
func postLimitedUpload(t *testing.T, app *fiber.App, path, bearer string, body *bytes.Buffer, contentType string) limitedUploadResponse {
	t.Helper()
	req := httptest.NewRequest(fiber.MethodPost, path, body)
	req.Header.Set("Content-Type", contentType)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 0})
	if err != nil {
		t.Fatal(err)
	}
	return readLimitedUpload(t, resp)
}

func readLimitedUpload(t *testing.T, resp *http.Response) limitedUploadResponse {
	t.Helper()
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("status %d body %s: %v", resp.StatusCode, raw, err)
	}
	if resp.StatusCode == fiber.StatusTooManyRequests && !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/problem+json") {
		t.Fatalf("429 as %s", resp.Header.Get("Content-Type"))
	}
	return limitedUploadResponse{status: resp.StatusCode, retryAfter: resp.Header.Get("Retry-After"), body: got}
}

// postPDFAs uploads a PDF of size bytes through POST /v1/media.
func postPDFAs(t *testing.T, app *fiber.App, bearer string, size int) limitedUploadResponse {
	t.Helper()
	body, contentType := pdfForm(t, size)
	return postLimitedUpload(t, app, "/v1/media", bearer, body, contentType)
}

// postProfilePictureAs uploads a PNG as the caller's profile picture.
func postProfilePictureAs(t *testing.T, app *fiber.App, bearer string) limitedUploadResponse {
	t.Helper()
	body, contentType := fileForm(t, "image", "me.png", pngPicture(t))
	return postLimitedUpload(t, app, "/v1/users/me/profile-picture", bearer, body, contentType)
}

func requireUploadStatus(t *testing.T, resp limitedUploadResponse, want int) {
	t.Helper()
	if resp.status != want {
		t.Fatalf("status %d body %v; want %d", resp.status, resp.body, want)
	}
}

// requireRateLimited checks a refused upload: 429 media_rate_limited naming
// the limit hit, with Retry-After in seconds matching the body.
func requireRateLimited(t *testing.T, resp limitedUploadResponse, limit string, retryAfter time.Duration) {
	t.Helper()
	seconds := strconv.Itoa(int(retryAfter / time.Second))
	if resp.status != fiber.StatusTooManyRequests || resp.body["code"] != "media_rate_limited" ||
		resp.body["status"] != float64(fiber.StatusTooManyRequests) || resp.body["limit"] != limit ||
		resp.retryAfter != seconds || resp.body["retryAfterSeconds"] != float64(retryAfter/time.Second) {
		t.Fatalf("status %d Retry-After %q body %v; want 429 %s after %s", resp.status, resp.retryAfter, resp.body, limit, seconds)
	}
}

// requireUploadBudget checks that a 429 states the person's whole budget,
// each maximum under its own member.
func requireUploadBudget(t *testing.T, resp limitedUploadResponse, maxUploads int, window time.Duration, maxDailyBytes int64) {
	t.Helper()
	want := map[string]any{
		"maxUploads":          float64(maxUploads),
		"uploadWindowSeconds": float64(window / time.Second),
		"maxDailyBytes":       float64(maxDailyBytes),
	}
	for member, value := range want {
		if resp.body[member] != value {
			t.Fatalf("problem %v; want %s %v", resp.body, member, value)
		}
	}
	for _, gone := range []string{"maxBytes", "windowSeconds"} {
		if _, ok := resp.body[gone]; ok {
			t.Fatalf("problem %v still carries %s", resp.body, gone)
		}
	}
}

func TestSingleStepUploadsShareACountPerRollingWindow(t *testing.T) {
	t.Parallel()
	keys := testauth.New(t)
	clock := newManualClock()
	app := limitedUploadApp(keys, media.NewUploadLimiter(media.UploadLimits{
		Count: 3, CountWindow: 10 * time.Minute, DailyBytes: 1 << 30,
	}, clock.Now))
	ada := newPersonBearer(t, keys)

	requireUploadStatus(t, postPDFAs(t, app, ada, 1024), fiber.StatusCreated)
	clock.Advance(time.Minute)
	requireUploadStatus(t, postProfilePictureAs(t, app, ada), fiber.StatusOK)
	requireUploadStatus(t, postPDFAs(t, app, ada, 1024), fiber.StatusCreated)
	clock.Advance(2 * time.Minute)

	refused := postProfilePictureAs(t, app, ada)
	requireRateLimited(t, refused, "uploads", 7*time.Minute)
	requireUploadBudget(t, refused, 3, 10*time.Minute, 1<<30)
	requireRateLimited(t, postPDFAs(t, app, ada, 1024), "uploads", 7*time.Minute)

	// The first upload leaves the window ten minutes after it was made.
	clock.Advance(7 * time.Minute)
	requireUploadStatus(t, postPDFAs(t, app, ada, 1024), fiber.StatusCreated)
	requireRateLimited(t, postPDFAs(t, app, ada, 1024), "uploads", time.Minute)
}

func TestSingleStepUploadsShareAVolumePerRollingDay(t *testing.T) {
	t.Parallel()
	keys := testauth.New(t)
	clock := newManualClock()
	cvBytes := pdfFormBytes(t, 10_000)
	// Two CVs and a little more: a third CV does not fit, a small picture does.
	daily := 2*cvBytes + cvBytes/2
	app := limitedUploadApp(keys, media.NewUploadLimiter(media.UploadLimits{
		Count: 100, CountWindow: 10 * time.Minute, DailyBytes: daily,
	}, clock.Now))
	ada := newPersonBearer(t, keys)

	requireUploadStatus(t, postPDFAs(t, app, ada, 10_000), fiber.StatusCreated)
	clock.Advance(time.Hour)
	requireUploadStatus(t, postPDFAs(t, app, ada, 10_000), fiber.StatusCreated)
	clock.Advance(time.Hour)

	refused := postPDFAs(t, app, ada, 10_000)
	requireRateLimited(t, refused, "volume", 22*time.Hour)
	requireUploadBudget(t, refused, 100, 10*time.Minute, daily)
	requireUploadStatus(t, postProfilePictureAs(t, app, ada), fiber.StatusOK)

	// The first CV leaves the day 24 hours after it was sent.
	clock.Advance(22 * time.Hour)
	requireUploadStatus(t, postPDFAs(t, app, ada, 10_000), fiber.StatusCreated)
}

func TestEachPersonHasTheirOwnUploadBudget(t *testing.T) {
	t.Parallel()
	keys := testauth.New(t)
	clock := newManualClock()
	app := limitedUploadApp(keys, media.NewUploadLimiter(media.UploadLimits{
		Count: 1, CountWindow: 10 * time.Minute, DailyBytes: 1 << 30,
	}, clock.Now))
	ada, grace := newPersonBearer(t, keys), newPersonBearer(t, keys)

	requireUploadStatus(t, postPDFAs(t, app, ada, 1024), fiber.StatusCreated)
	requireRateLimited(t, postProfilePictureAs(t, app, ada), "uploads", 10*time.Minute)

	requireUploadStatus(t, postProfilePictureAs(t, app, grace), fiber.StatusOK)
	requireRateLimited(t, postPDFAs(t, app, grace, 1024), "uploads", 10*time.Minute)
}

func TestAnonymousUploadsAreRefusedAsUnauthorizedNotRateLimited(t *testing.T) {
	t.Parallel()
	keys := testauth.New(t)
	clock := newManualClock()
	app := limitedUploadApp(keys, media.NewUploadLimiter(media.UploadLimits{
		Count: 1, CountWindow: 10 * time.Minute, DailyBytes: 1,
	}, clock.Now))

	for range 3 {
		requireUploadStatus(t, postPDFAs(t, app, "", 1024), fiber.StatusUnauthorized)
		requireUploadStatus(t, postProfilePictureAs(t, app, ""), fiber.StatusUnauthorized)
	}
}

// A body sent without Content-Length (chunked) counts the bytes that
// arrived. app.Test always writes a Content-Length, so the request goes
// through a listener instead.
func TestAnUploadWithoutContentLengthCountsTheBytesThatArrived(t *testing.T) {
	t.Parallel()
	keys := testauth.New(t)
	clock := newManualClock()
	cvBytes := pdfFormBytes(t, 10_000)
	app := limitedUploadApp(keys, media.NewUploadLimiter(media.UploadLimits{
		Count: 100, CountWindow: 10 * time.Minute, DailyBytes: cvBytes + cvBytes/2,
	}, clock.Now))
	ln := fasthttputil.NewInmemoryListener()
	go func() { _ = app.Listener(ln, fiber.ListenConfig{DisableStartupMessage: true}) }()
	t.Cleanup(func() { _ = app.Shutdown() })
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(context.Context, string, string) (net.Conn, error) { return ln.Dial() },
	}}
	ada := newPersonBearer(t, keys)

	chunked := func() limitedUploadResponse {
		t.Helper()
		body, contentType := pdfForm(t, 10_000)
		req, err := http.NewRequest(fiber.MethodPost, "http://core.test/v1/media", io.NopCloser(body))
		if err != nil {
			t.Fatal(err)
		}
		req.ContentLength = -1
		req.TransferEncoding = []string{"chunked"}
		req.Header.Set("Content-Type", contentType)
		req.Header.Set("Authorization", "Bearer "+ada)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return readLimitedUpload(t, resp)
	}

	requireUploadStatus(t, chunked(), fiber.StatusCreated)
	requireRateLimited(t, chunked(), "volume", 24*time.Hour)
}

// When both limits refuse an upload, the problem names the one that lasts
// longer: retrying when the other ends would only be refused again.
func TestAnUploadOverBothLimitsNamesTheLongerWait(t *testing.T) {
	t.Parallel()
	keys := testauth.New(t)
	clock := newManualClock()
	cvBytes := pdfFormBytes(t, 10_000)
	app := limitedUploadApp(keys, media.NewUploadLimiter(media.UploadLimits{
		Count: 2, CountWindow: 10 * time.Minute, DailyBytes: cvBytes + cvBytes/2,
	}, clock.Now))
	ada := newPersonBearer(t, keys)

	requireUploadStatus(t, postPDFAs(t, app, ada, 10_000), fiber.StatusCreated)
	clock.Advance(time.Minute)
	requireUploadStatus(t, postProfilePictureAs(t, app, ada), fiber.StatusOK)
	clock.Advance(time.Minute)

	requireRateLimited(t, postPDFAs(t, app, ada, 10_000), "volume", 24*time.Hour-2*time.Minute)
}

// faultyBlob is storage that can fail or panic on write, as R2 can fail.
type faultyBlob struct {
	*media.MemoryBlob
	mu    sync.Mutex
	fault string // "", "fail" or "panic"
}

func (b *faultyBlob) set(fault string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.fault = fault
}

func (b *faultyBlob) Put(ctx context.Context, key string, data []byte, meta media.BlobMetadata) error {
	b.mu.Lock()
	fault := b.fault
	b.mu.Unlock()
	switch fault {
	case "fail":
		return errors.New("storage unavailable")
	case "panic":
		panic("storage client bug")
	}
	return b.MemoryBlob.Put(ctx, key, data, meta)
}

// An upload core fails to store is given back: count and bytes. Room for
// exactly one CV proves both come back.
func TestAnUploadThatEndsInAServerErrorIsRefunded(t *testing.T) {
	t.Parallel()
	for _, fault := range []string{"fail", "panic"} {
		t.Run(fault, func(t *testing.T) {
			t.Parallel()
			keys := testauth.New(t)
			clock := newManualClock()
			cvBytes := pdfFormBytes(t, 10_000)
			blobs := &faultyBlob{MemoryBlob: media.NewMemoryBlob()}
			app := limitedUploadAppWithBlobs(keys, media.NewUploadLimiter(media.UploadLimits{
				Count: 1, CountWindow: 10 * time.Minute, DailyBytes: cvBytes + cvBytes/2,
			}, clock.Now), blobs)
			ada := newPersonBearer(t, keys)

			blobs.set(fault)
			requireUploadStatus(t, postPDFAs(t, app, ada, 10_000), fiber.StatusInternalServerError)
			blobs.set("")
			requireUploadStatus(t, postPDFAs(t, app, ada, 10_000), fiber.StatusCreated)
			requireRateLimited(t, postPDFAs(t, app, ada, 10_000), "volume", 24*time.Hour)
		})
	}
}

// A refused upload stays charged: its bytes were received, and free refusals
// would let anyone send junk without end.
func TestAnUploadRefusedAsTheCallersFaultStaysCharged(t *testing.T) {
	t.Parallel()
	keys := testauth.New(t)
	clock := newManualClock()
	cvBytes := pdfFormBytes(t, 10_000)
	app := limitedUploadApp(keys, media.NewUploadLimiter(media.UploadLimits{
		Count: 2, CountWindow: 10 * time.Minute, DailyBytes: cvBytes + cvBytes/2,
	}, clock.Now))
	ada, grace := newPersonBearer(t, keys), newPersonBearer(t, keys)

	// A PDF is not a profile picture: 415, and the CV's bytes are spent.
	pdf, contentType := pdfForm(t, 10_000)
	requireUploadStatus(t, postLimitedUpload(t, app, "/v1/users/me/profile-picture", ada, pdf, contentType), fiber.StatusUnsupportedMediaType)
	requireRateLimited(t, postPDFAs(t, app, ada, 10_000), "volume", 24*time.Hour)

	// A form without its file: 400, and the upload is counted.
	empty, contentType := fileForm(t, "attachment", "cv.pdf", []byte("%PDF-1.7\n"))
	requireUploadStatus(t, postLimitedUpload(t, app, "/v1/media", grace, empty, contentType), fiber.StatusBadRequest)
	requireUploadStatus(t, postProfilePictureAs(t, app, grace), fiber.StatusOK)
	requireRateLimited(t, postProfilePictureAs(t, app, grace), "uploads", 10*time.Minute)
}

// storeSpy is the Media service, noting each file it is asked to store.
type storeSpy struct {
	media.Service
	mu     sync.Mutex
	stored int
}

func (s *storeSpy) note() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stored++
}

func (s *storeSpy) take() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored := s.stored
	s.stored = 0
	return stored
}

func (s *storeSpy) Upload(ctx context.Context, p authz.Principal, name, contentType string, data []byte) (media.Media, error) {
	s.note()
	return s.Service.Upload(ctx, p, name, contentType, data)
}

func (s *storeSpy) UploadForPurpose(ctx context.Context, p authz.Principal, purpose string, file media.UploadedFile) (media.Media, error) {
	s.note()
	return s.Service.UploadForPurpose(ctx, p, purpose, file)
}

var routeParam = regexp.MustCompile(`:[A-Za-z]+`)

// routesThatStore sends every POST, PUT and PATCH route of the assembled app
// an upload an organizer may make, and names the routes that stored it.
func routesThatStore(t *testing.T, keys *testauth.Bundle, limiter *media.UploadLimiter) []string {
	t.Helper()
	deps := memoryDeps()
	spy := &storeSpy{Service: deps.Media}
	deps.Media = spy
	deps.ParseToken = keys.Parse()
	deps.MediaUploadLimiter = limiter
	app := httpx.New(deps)
	organizer := keys.Token(t, jwt.MapClaims{
		"sub": uuid.NewString(), "email": "yk@example.com", "given_name": "Y", "family_name": "K", "groups": []string{"/UYELER/YK"},
	})
	picture := pngPicture(t)

	var stored []string
	for _, route := range app.GetRoutes(true) {
		if route.Method != fiber.MethodPost && route.Method != fiber.MethodPut && route.Method != fiber.MethodPatch {
			continue
		}
		var body bytes.Buffer
		form := multipart.NewWriter(&body)
		if err := form.WriteField("purpose", "event_cover"); err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{"file", "image"} {
			part, err := form.CreateFormFile(field, "cover.png")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := part.Write(picture); err != nil {
				t.Fatal(err)
			}
		}
		if err := form.Close(); err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(route.Method, routeParam.ReplaceAllString(route.Path, uuid.NewString()), &body)
		req.Header.Set("Content-Type", form.FormDataContentType())
		req.Header.Set("Authorization", "Bearer "+organizer)
		resp, err := app.Test(req, fiber.TestConfig{Timeout: 5 * time.Second})
		if err != nil {
			t.Fatalf("%s %s: %v", route.Method, route.Path, err)
		}
		resp.Body.Close()
		if spy.take() > 0 {
			stored = append(stored, route.Method+" "+route.Path)
		}
	}
	return stored
}

// Every route that stores a file sent through core is charged to the
// person's budget: with a budget that refuses everything, none stores one.
// A new upload route that skips the limiter fails here.
func TestEveryRouteThatStoresAFileIsChargedToTheUploadBudget(t *testing.T) {
	t.Parallel()
	keys := testauth.New(t)

	open := routesThatStore(t, keys, media.NewUploadLimiter(media.DefaultUploadLimits(), time.Now))
	if !slices.Contains(open, "POST /v1/media") || !slices.Contains(open, "POST /v1/users/me/profile-picture") {
		t.Fatalf("the probe stored through %v; it must reach both upload routes", open)
	}

	shut := media.NewUploadLimiter(media.UploadLimits{Count: 1, CountWindow: time.Minute, DailyBytes: 0}, time.Now)
	if stored := routesThatStore(t, keys, shut); len(stored) > 0 {
		t.Fatalf("stored a file without charging the upload budget: %v", stored)
	}
}
