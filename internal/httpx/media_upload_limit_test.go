package httpx_test

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
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

// testClock is the upload limiter's clock, moved by hand.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newTestClock() *testClock {
	return &testClock{now: time.Date(2026, 9, 26, 9, 0, 0, 0, time.UTC)}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func uploadLimitApp(keys *testauth.Bundle, limiter *media.UploadLimiter) *fiber.App {
	az := authz.NewAuthorizer(authz.DefaultPolicy())
	return httpx.New(httpx.Deps{
		Users:              user.NewService(user.NewMemoryStore()),
		Media:              media.NewService(media.NewMemoryStore(), media.NewMemoryBlob(), az, ""),
		ParseToken:         keys.Parse(),
		MediaUploadLimiter: limiter,
		TrustedProxies:     testTrustedProxies(),
	})
}

// person is a signed-in caller's bearer token.
func person(t *testing.T, keys *testauth.Bundle) string {
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

// pdfOfSize is a PDF-looking file of exactly size bytes.
func pdfOfSize(size int) []byte {
	pdf := make([]byte, size)
	copy(pdf, "%PDF-1.7\n")
	return pdf
}

type limitResponse struct {
	status     int
	retryAfter string
	body       map[string]any
}

// uploadForm is a multipart form carrying file as its field.
func uploadForm(t *testing.T, field, name string, file []byte) (*bytes.Buffer, string) {
	t.Helper()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, err := form.CreateFormFile(field, name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(file); err != nil {
		t.Fatal(err)
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	return &body, form.FormDataContentType()
}

// upload posts file as a multipart form to path; an empty token is anonymous.
func upload(t *testing.T, app *fiber.App, path, token, field, name string, file []byte) limitResponse {
	t.Helper()
	body, contentType := uploadForm(t, field, name, file)
	req := httptest.NewRequest(fiber.MethodPost, path, body)
	req.Header.Set("Content-Type", contentType)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("%s: status %d body %s: %v", path, resp.StatusCode, raw, err)
	}
	if resp.StatusCode == fiber.StatusTooManyRequests && !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/problem+json") {
		t.Fatalf("429 as %s", resp.Header.Get("Content-Type"))
	}
	return limitResponse{status: resp.StatusCode, retryAfter: resp.Header.Get("Retry-After"), body: got}
}

func uploadMedia(t *testing.T, app *fiber.App, token string, file []byte) limitResponse {
	t.Helper()
	return upload(t, app, "/v1/media", token, "file", "cv.pdf", file)
}

func uploadProfilePicture(t *testing.T, app *fiber.App, token string) limitResponse {
	t.Helper()
	return upload(t, app, "/v1/users/me/profile-picture", token, "image", "me.png", pngPicture(t))
}

func requireStatus(t *testing.T, resp limitResponse, want int) {
	t.Helper()
	if resp.status != want {
		t.Fatalf("status %d body %v; want %d", resp.status, resp.body, want)
	}
}

// requireRateLimited checks a refused upload: 429 media_rate_limited naming
// the limit hit, with Retry-After in seconds matching the body.
func requireRateLimited(t *testing.T, resp limitResponse, limit string, retryAfter time.Duration) {
	t.Helper()
	seconds := strconv.Itoa(int(retryAfter / time.Second))
	if resp.status != fiber.StatusTooManyRequests || resp.body["code"] != "media_rate_limited" ||
		resp.body["status"] != float64(fiber.StatusTooManyRequests) || resp.body["limit"] != limit ||
		resp.retryAfter != seconds || resp.body["retryAfterSeconds"] != float64(retryAfter/time.Second) {
		t.Fatalf("status %d Retry-After %q body %v; want 429 %s after %s", resp.status, resp.retryAfter, resp.body, limit, seconds)
	}
}

func TestSingleStepUploadsShareACountPerRollingWindow(t *testing.T) {
	t.Parallel()
	keys := testauth.New(t)
	clock := newTestClock()
	app := uploadLimitApp(keys, media.NewUploadLimiter(media.UploadLimits{
		Count: 3, CountWindow: 10 * time.Minute, DailyBytes: 1 << 30,
	}, clock.Now))
	ada := person(t, keys)

	requireStatus(t, uploadMedia(t, app, ada, pdfOfSize(1024)), fiber.StatusCreated)
	clock.Advance(time.Minute)
	requireStatus(t, uploadProfilePicture(t, app, ada), fiber.StatusOK)
	requireStatus(t, uploadMedia(t, app, ada, pdfOfSize(1024)), fiber.StatusCreated)
	clock.Advance(2 * time.Minute)

	refused := uploadProfilePicture(t, app, ada)
	requireRateLimited(t, refused, "uploads", 7*time.Minute)
	if refused.body["maxUploads"] != float64(3) || refused.body["windowSeconds"] != float64(600) {
		t.Fatalf("problem %v", refused.body)
	}
	requireRateLimited(t, uploadMedia(t, app, ada, pdfOfSize(1024)), "uploads", 7*time.Minute)

	// The first upload leaves the window ten minutes after it was made.
	clock.Advance(7 * time.Minute)
	requireStatus(t, uploadMedia(t, app, ada, pdfOfSize(1024)), fiber.StatusCreated)
	requireRateLimited(t, uploadMedia(t, app, ada, pdfOfSize(1024)), "uploads", time.Minute)
}

func TestSingleStepUploadsShareAVolumePerRollingDay(t *testing.T) {
	t.Parallel()
	keys := testauth.New(t)
	clock := newTestClock()
	cv := pdfOfSize(10_000)
	form, _ := uploadForm(t, "file", "cv.pdf", cv)
	cvBytes := int64(form.Len())
	// Two CVs and a little more: a third CV does not fit, a small picture does.
	daily := 2*cvBytes + cvBytes/2
	app := uploadLimitApp(keys, media.NewUploadLimiter(media.UploadLimits{
		Count: 100, CountWindow: 10 * time.Minute, DailyBytes: daily,
	}, clock.Now))
	ada := person(t, keys)

	requireStatus(t, uploadMedia(t, app, ada, cv), fiber.StatusCreated)
	clock.Advance(time.Hour)
	requireStatus(t, uploadMedia(t, app, ada, cv), fiber.StatusCreated)
	clock.Advance(time.Hour)

	refused := uploadMedia(t, app, ada, cv)
	requireRateLimited(t, refused, "volume", 22*time.Hour)
	if refused.body["maxBytes"] != float64(daily) || refused.body["windowSeconds"] != float64(24*60*60) {
		t.Fatalf("problem %v", refused.body)
	}
	requireStatus(t, uploadProfilePicture(t, app, ada), fiber.StatusOK)

	// The first CV leaves the day 24 hours after it was sent.
	clock.Advance(22 * time.Hour)
	requireStatus(t, uploadMedia(t, app, ada, cv), fiber.StatusCreated)
}

func TestEachPersonHasTheirOwnUploadBudget(t *testing.T) {
	t.Parallel()
	keys := testauth.New(t)
	clock := newTestClock()
	app := uploadLimitApp(keys, media.NewUploadLimiter(media.UploadLimits{
		Count: 1, CountWindow: 10 * time.Minute, DailyBytes: 1 << 30,
	}, clock.Now))
	ada, grace := person(t, keys), person(t, keys)

	requireStatus(t, uploadMedia(t, app, ada, pdfOfSize(1024)), fiber.StatusCreated)
	requireRateLimited(t, uploadProfilePicture(t, app, ada), "uploads", 10*time.Minute)

	requireStatus(t, uploadProfilePicture(t, app, grace), fiber.StatusOK)
	requireRateLimited(t, uploadMedia(t, app, grace, pdfOfSize(1024)), "uploads", 10*time.Minute)
}

func TestAnonymousUploadsAreRefusedAsUnauthorizedNotRateLimited(t *testing.T) {
	t.Parallel()
	keys := testauth.New(t)
	clock := newTestClock()
	app := uploadLimitApp(keys, media.NewUploadLimiter(media.UploadLimits{
		Count: 1, CountWindow: 10 * time.Minute, DailyBytes: 1,
	}, clock.Now))

	for range 3 {
		requireStatus(t, uploadMedia(t, app, "", pdfOfSize(1024)), fiber.StatusUnauthorized)
		requireStatus(t, upload(t, app, "/v1/users/me/profile-picture", "", "image", "me.png", pngPicture(t)), fiber.StatusUnauthorized)
	}
}

// A body sent without Content-Length (chunked) counts the bytes that
// arrived. app.Test always writes a Content-Length, so the request goes
// through a listener instead.
func TestAnUploadWithoutContentLengthCountsTheBytesThatArrived(t *testing.T) {
	t.Parallel()
	keys := testauth.New(t)
	clock := newTestClock()
	cv := pdfOfSize(10_000)
	form, _ := uploadForm(t, "file", "cv.pdf", cv)
	cvBytes := int64(form.Len())
	app := uploadLimitApp(keys, media.NewUploadLimiter(media.UploadLimits{
		Count: 100, CountWindow: 10 * time.Minute, DailyBytes: cvBytes + cvBytes/2,
	}, clock.Now))
	ln := fasthttputil.NewInmemoryListener()
	go func() { _ = app.Listener(ln, fiber.ListenConfig{DisableStartupMessage: true}) }()
	t.Cleanup(func() { _ = app.Shutdown() })
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(context.Context, string, string) (net.Conn, error) { return ln.Dial() },
	}}
	ada := person(t, keys)

	chunked := func() limitResponse {
		t.Helper()
		body, contentType := uploadForm(t, "file", "cv.pdf", cv)
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
		defer resp.Body.Close()
		var got map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		return limitResponse{status: resp.StatusCode, retryAfter: resp.Header.Get("Retry-After"), body: got}
	}

	requireStatus(t, chunked(), fiber.StatusCreated)
	requireRateLimited(t, chunked(), "volume", 24*time.Hour)
}

// When both limits refuse an upload, the problem names the one that lasts
// longer: retrying when the other ends would only be refused again.
func TestAnUploadOverBothLimitsNamesTheLongerWait(t *testing.T) {
	t.Parallel()
	keys := testauth.New(t)
	clock := newTestClock()
	cv := pdfOfSize(10_000)
	form, _ := uploadForm(t, "file", "cv.pdf", cv)
	cvBytes := int64(form.Len())
	app := uploadLimitApp(keys, media.NewUploadLimiter(media.UploadLimits{
		Count: 2, CountWindow: 10 * time.Minute, DailyBytes: cvBytes + cvBytes/2,
	}, clock.Now))
	ada := person(t, keys)

	requireStatus(t, uploadMedia(t, app, ada, cv), fiber.StatusCreated)
	clock.Advance(time.Minute)
	requireStatus(t, uploadProfilePicture(t, app, ada), fiber.StatusOK)
	clock.Advance(time.Minute)

	requireRateLimited(t, uploadMedia(t, app, ada, cv), "volume", 24*time.Hour-2*time.Minute)
}
