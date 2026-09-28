package httpx_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/config"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/httpx"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/media/s3test"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testauth"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// partSize is the part size core hands out.
const partSize = 16 << 20

// directCatalogue is the reviewed catalogue with club_file and video open:
// no malware scan (core has no scanner until ticket 12, so the reviewed
// club_file is refused) and attached by core (where club files and videos
// are attached is not settled, so the reviewed ones name no product).
func directCatalogue(t testing.TB) media.Catalogue {
	t.Helper()
	var file map[string]any
	if err := json.Unmarshal(config.MediaPurposes, &file); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"club_file", "video"} {
		purpose := file["purposes"].(map[string]any)[name].(map[string]any)
		purpose["scan"] = false
		purpose["attach"] = "core"
	}
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

// directEnv is the assembled app with Direct upload: real PostgreSQL, R2
// against a fake S3, and the upload budget on a clock the test moves.
type directEnv struct {
	app     *fiber.App
	keys    *testauth.Bundle
	s3      *s3test.Server
	r2      *media.R2
	store   *media.PostgresStore
	clock   *manualClock
	limiter *media.UploadLimiter
}

func newDirectEnv(t *testing.T, limits media.UploadLimits) *directEnv {
	t.Helper()
	pool := testpostgres.Start(t)
	if err := migrate.Apply(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	fake := s3test.New(t)
	r2 := media.NewR2(media.R2Config{Endpoint: fake.URL, AccessKey: "access", SecretKey: "secret", Bucket: "media"})
	clock := &manualClock{now: time.Now().UTC().Truncate(time.Second)}
	limiter := media.NewUploadLimiter(limits, clock.Now)
	store := media.NewPostgresStore(pool)
	keys := testauth.New(t)
	deps := memoryDeps()
	deps.Users = user.NewService(user.NewPostgresStore(pool))
	deps.ParseToken = keys.Parse()
	deps.MediaUploadLimiter = limiter
	deps.Media = media.NewServiceWithOptions(store, r2, authz.NewAuthorizer(authz.DefaultPolicy()), "https://cdn.example.test",
		media.ServiceOptions{
			Catalogue:       directCatalogue(t),
			ServiceProducts: deps.ServiceClients.Products(),
			Direct:          media.DirectUploadConfig{Storage: r2, Limiter: limiter, Now: clock.Now},
		})
	return &directEnv{app: httpx.New(deps), keys: keys, s3: fake, r2: r2, store: store, clock: clock, limiter: limiter}
}

// organizerToken is an organizer's token: someone who may create Events,
// so the club_file purpose's event_editor rule lets them upload.
func organizerToken(t *testing.T, keys *testauth.Bundle) string {
	t.Helper()
	return keys.Token(t, jwt.MapClaims{
		"sub": uuid.NewString(), "email": uuid.NewString() + "@example.com", "given_name": "Y", "family_name": "K",
		"groups": []string{"/UYELER/YK"},
	})
}

func startBody(purpose, name string, size int) string {
	return fmt.Sprintf(`{"purpose":%q,"name":%q,"size":%d}`, purpose, name, size)
}

type sentPart struct {
	PartNumber int    `json:"partNumber"`
	ETag       string `json:"etag"`
}

// partsOf reads the part addresses of an upload answer.
func partsOf(t *testing.T, upload map[string]any) []map[string]any {
	t.Helper()
	raw, ok := upload["parts"].([]any)
	if !ok {
		t.Fatalf("no parts in %v", upload)
	}
	parts := make([]map[string]any, 0, len(raw))
	for _, p := range raw {
		parts = append(parts, p.(map[string]any))
	}
	return parts
}

// sendParts PUTs each part of file to its address, as the browser does,
// and returns the parts with the ETags storage answered.
func sendParts(t *testing.T, parts []map[string]any, file []byte) []sentPart {
	t.Helper()
	var sent []sentPart
	for _, p := range parts {
		number := int(p["partNumber"].(float64))
		size := int(p["size"].(float64))
		from := (number - 1) * partSize
		sent = append(sent, sentPart{PartNumber: number, ETag: putPartBytes(t, p["url"].(string), file[from:from+size])})
	}
	return sent
}

func putPartBytes(t *testing.T, address string, data []byte) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, address, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("part PUT: status %d", resp.StatusCode)
	}
	return resp.Header.Get("ETag")
}

func completeBody(t *testing.T, parts []sentPart) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"parts": parts})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// pdfFile is a file of size bytes that starts like a PDF.
func pdfFile(size int) []byte {
	file := bytes.Repeat([]byte{'x'}, size)
	copy(file, "%PDF-1.7\n")
	return file
}

// zipFile is a real ZIP archive of about size bytes (stored, not deflated).
func zipFile(t *testing.T, size int) []byte {
	t.Helper()
	var out bytes.Buffer
	archive := zip.NewWriter(&out)
	w, err := archive.CreateHeader(&zip.FileHeader{Name: "data.bin", Method: zip.Store})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(bytes.Repeat([]byte{7}, size)); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// upload runs a whole Direct upload of file for the purpose and answers the
// completion.
func (e *directEnv) upload(t *testing.T, token, purpose, name string, file []byte) jsonResponse {
	t.Helper()
	started := sendJSON(t, e.app, token, fiber.MethodPost, "/v1/uploads", startBody(purpose, name, len(file)))
	if started.status != fiber.StatusCreated {
		t.Fatalf("start: status %d body %v", started.status, started.body)
	}
	sent := sendParts(t, partsOf(t, started.body), file)
	return sendJSON(t, e.app, token, fiber.MethodPost, "/v1/uploads/"+started.body["id"].(string)+"/complete", completeBody(t, sent))
}

func TestDirectUploadCompletesIntoAMediaHTTP(t *testing.T) {
	e := newDirectEnv(t, media.DefaultUploadLimits())
	organizer := organizerToken(t, e.keys)

	t.Run("a PDF in two parts", func(t *testing.T) {
		file := pdfFile(partSize + 1234)
		started := sendJSON(t, e.app, organizer, fiber.MethodPost, "/v1/uploads", startBody("club_file", "etkinlik notları.pdf", len(file)))
		if started.status != fiber.StatusCreated {
			t.Fatalf("start: status %d body %v", started.status, started.body)
		}
		parts := partsOf(t, started.body)
		if started.body["partSize"] != float64(partSize) || started.body["partCount"] != float64(2) || len(parts) != 2 ||
			parts[0]["size"] != float64(partSize) || parts[1]["size"] != float64(1234) || started.body["purpose"] != "club_file" {
			t.Fatalf("start: %v", started.body)
		}
		id := started.body["id"].(string)
		if open := e.s3.OpenUploads("media"); !slices.Equal(open, []string{"pending/" + id}) {
			t.Fatalf("open uploads %v", open)
		}

		sent := sendParts(t, parts, file)
		done := sendJSON(t, e.app, organizer, fiber.MethodPost, "/v1/uploads/"+id+"/complete", completeBody(t, sent))
		if done.status != fiber.StatusCreated || done.body["id"] != id || done.body["purpose"] != "club_file" ||
			done.body["status"] != "pending" || done.body["type"] != "application/pdf" || done.body["size"] != float64(len(file)) ||
			done.body["name"] != "etkinlik notları.pdf" || done.body["expiresAt"] == nil {
			t.Fatalf("complete: status %d body %v", done.status, done.body)
		}
		address, _ := done.body["url"].(string)
		key, found := strings.CutPrefix(address, "https://cdn.example.test/")
		if !found || !strings.HasPrefix(key, "files/") {
			t.Fatalf("address %q", address)
		}
		stored, ok := e.s3.Object("media", key)
		if !ok || !bytes.Equal(stored.Data, file) || stored.ContentType != "application/pdf" || stored.ContentDisposition != "" {
			t.Fatalf("stored object: found %v, type %q, disposition %q", ok, stored.ContentType, stored.ContentDisposition)
		}
		if _, pending := e.s3.Object("media", "pending/"+id); pending || len(e.s3.OpenUploads("media")) != 0 {
			t.Fatalf("pending object left: %v, open uploads %v", pending, e.s3.OpenUploads("media"))
		}
		if got := sendJSON(t, e.app, organizer, fiber.MethodGet, "/v1/media/"+id, ""); got.status != fiber.StatusOK || got.body["purpose"] != "club_file" {
			t.Fatalf("media: status %d body %v", got.status, got.body)
		}

		// A completion retried after its answer was lost gets the same Media.
		again := sendJSON(t, e.app, organizer, fiber.MethodPost, "/v1/uploads/"+id+"/complete", completeBody(t, sent))
		if again.status != fiber.StatusCreated || again.body["id"] != id || again.body["url"] != address {
			t.Fatalf("retried completion: status %d body %v", again.status, again.body)
		}
		// Nothing is left to send.
		if parts := sendJSON(t, e.app, organizer, fiber.MethodPost, "/v1/uploads/"+id+"/parts", ""); parts.status != fiber.StatusNotFound {
			t.Fatalf("parts of a completed upload: status %d", parts.status)
		}
	})

	t.Run("a ZIP is a club file, served only as a download", func(t *testing.T) {
		file := zipFile(t, 3000)
		done := e.upload(t, organizer, "club_file", "veri seti.zip", file)
		if done.status != fiber.StatusCreated || done.body["type"] != "application/zip" {
			t.Fatalf("complete: status %d body %v", done.status, done.body)
		}
		key := strings.TrimPrefix(done.body["url"].(string), "https://cdn.example.test/")
		stored, ok := e.s3.Object("media", key)
		if !ok || stored.ContentType != "application/octet-stream" ||
			stored.ContentDisposition != `attachment; filename="veri seti.zip"` {
			t.Fatalf("stored ZIP: found %v, type %q, disposition %q", ok, stored.ContentType, stored.ContentDisposition)
		}
	})

	t.Run("an MP4 is known by its ftyp box", func(t *testing.T) {
		file := make([]byte, 5000)
		copy(file, "\x00\x00\x00\x18ftypisom\x00\x00\x02\x00isomiso2")
		done := e.upload(t, organizer, "video", "kayıt.mp4", file)
		if done.status != fiber.StatusCreated || done.body["type"] != "video/mp4" || done.body["purpose"] != "video" {
			t.Fatalf("complete: status %d body %v", done.status, done.body)
		}
		// A PDF is not a video.
		refused := e.upload(t, organizer, "video", "kayıt.mp4", pdfFile(5000))
		requireCode(t, refused, fiber.StatusUnsupportedMediaType, "media_type_not_allowed")
	})

	t.Run("an interrupted upload continues where it stopped", func(t *testing.T) {
		file := pdfFile(2*partSize + 10)
		started := sendJSON(t, e.app, organizer, fiber.MethodPost, "/v1/uploads", startBody("club_file", "kayıt.pdf", len(file)))
		if started.status != fiber.StatusCreated {
			t.Fatalf("start: status %d body %v", started.status, started.body)
		}
		id := started.body["id"].(string)
		parts := partsOf(t, started.body)
		// The connection drops after the second part.
		first := sendParts(t, parts[1:2], file)

		resumed := sendJSON(t, e.app, organizer, fiber.MethodPost, "/v1/uploads/"+id+"/parts", "")
		if resumed.status != fiber.StatusOK {
			t.Fatalf("parts: status %d body %v", resumed.status, resumed.body)
		}
		uploaded, _ := resumed.body["uploaded"].([]any)
		missing := partsOf(t, resumed.body)
		if len(uploaded) != 1 || uploaded[0].(map[string]any)["partNumber"] != float64(2) ||
			uploaded[0].(map[string]any)["etag"] != first[0].ETag ||
			len(missing) != 2 || missing[0]["partNumber"] != float64(1) || missing[1]["partNumber"] != float64(3) {
			t.Fatalf("resumed: %v", resumed.body)
		}
		rest := sendParts(t, missing, file)
		all := []sentPart{rest[0], first[0], rest[1]}
		done := sendJSON(t, e.app, organizer, fiber.MethodPost, "/v1/uploads/"+id+"/complete", completeBody(t, all))
		if done.status != fiber.StatusCreated || done.body["size"] != float64(len(file)) {
			t.Fatalf("complete: status %d body %v", done.status, done.body)
		}
	})
}

func TestDirectUploadRefusalsHTTP(t *testing.T) {
	e := newDirectEnv(t, media.DefaultUploadLimits())
	organizer := organizerToken(t, e.keys)

	start := func(t *testing.T, file []byte) (string, []map[string]any) {
		t.Helper()
		started := sendJSON(t, e.app, organizer, fiber.MethodPost, "/v1/uploads", startBody("club_file", "dosya.pdf", len(file)))
		if started.status != fiber.StatusCreated {
			t.Fatalf("start: status %d body %v", started.status, started.body)
		}
		return started.body["id"].(string), partsOf(t, started.body)
	}
	// ended checks that a refused upload is gone, with nothing of it left
	// in storage.
	ended := func(t *testing.T, id string, sent []sentPart) {
		t.Helper()
		if _, pending := e.s3.Object("media", "pending/"+id); pending || slices.Contains(e.s3.OpenUploads("media"), "pending/"+id) {
			t.Fatalf("the refused upload left its pending object (%v) or parts (%v)", pending, e.s3.OpenUploads("media"))
		}
		if again := sendJSON(t, e.app, organizer, fiber.MethodPost, "/v1/uploads/"+id+"/complete", completeBody(t, sent)); again.status != fiber.StatusNotFound {
			t.Fatalf("completing a refused upload again: status %d body %v", again.status, again.body)
		}
	}

	t.Run("a file of another size than declared", func(t *testing.T) {
		file := pdfFile(2000)
		id, parts := start(t, file)
		// The part is one byte longer than its address was signed for;
		// R2 refuses that PUT, the fake takes it, and core refuses the file.
		sent := []sentPart{{PartNumber: 1, ETag: putPartBytes(t, parts[0]["url"].(string), append(file, 'x'))}}
		done := sendJSON(t, e.app, organizer, fiber.MethodPost, "/v1/uploads/"+id+"/complete", completeBody(t, sent))
		requireCode(t, done, fiber.StatusUnprocessableEntity, "upload_size_mismatch")
		if done.body["declaredSize"] != float64(2000) || done.body["size"] != float64(2001) {
			t.Fatalf("problem %v", done.body)
		}
		ended(t, id, sent)
	})

	t.Run("a file whose first bytes are not a type the purpose accepts", func(t *testing.T) {
		file := pngPicture(t)
		id, parts := start(t, file)
		sent := sendParts(t, parts, file)
		done := sendJSON(t, e.app, organizer, fiber.MethodPost, "/v1/uploads/"+id+"/complete", completeBody(t, sent))
		requireCode(t, done, fiber.StatusUnsupportedMediaType, "media_type_not_allowed")
		allowed, _ := done.body["allowedTypes"].([]any)
		if done.body["purpose"] != "club_file" || len(allowed) != 2 {
			t.Fatalf("problem %v", done.body)
		}
		ended(t, id, sent)
	})

	t.Run("narrowed limits the file does not meet", func(t *testing.T) {
		file := zipFile(t, 100)
		started := sendJSON(t, e.app, organizer, fiber.MethodPost, "/v1/uploads",
			fmt.Sprintf(`{"purpose":"club_file","name":"a.zip","size":%d,"limits":{"types":["application/pdf"]}}`, len(file)))
		if started.status != fiber.StatusCreated {
			t.Fatalf("start: status %d body %v", started.status, started.body)
		}
		id := started.body["id"].(string)
		sent := sendParts(t, partsOf(t, started.body), file)
		done := sendJSON(t, e.app, organizer, fiber.MethodPost, "/v1/uploads/"+id+"/complete", completeBody(t, sent))
		requireCode(t, done, fiber.StatusUnsupportedMediaType, "media_type_not_allowed")
		if allowed, _ := done.body["allowedTypes"].([]any); len(allowed) != 1 || allowed[0] != "application/pdf" {
			t.Fatalf("problem %v", done.body)
		}
		ended(t, id, sent)
	})

	t.Run("parts that are not the stored ones leave the upload open", func(t *testing.T) {
		file := pdfFile(3000)
		id, parts := start(t, file)
		sent := sendParts(t, parts, file)
		wrong := sendJSON(t, e.app, organizer, fiber.MethodPost, "/v1/uploads/"+id+"/complete",
			completeBody(t, []sentPart{{PartNumber: 1, ETag: `"0123"`}}))
		requireCode(t, wrong, fiber.StatusBadRequest, "upload_parts_mismatch")
		none := sendJSON(t, e.app, organizer, fiber.MethodPost, "/v1/uploads/"+id+"/complete", `{"parts":[]}`)
		requireCode(t, none, fiber.StatusBadRequest, "upload_parts_mismatch")
		if done := sendJSON(t, e.app, organizer, fiber.MethodPost, "/v1/uploads/"+id+"/complete", completeBody(t, sent)); done.status != fiber.StatusCreated {
			t.Fatalf("complete with the right parts: status %d body %v", done.status, done.body)
		}
	})

	t.Run("someone else's upload does not exist for them", func(t *testing.T) {
		file := pdfFile(1000)
		id, parts := start(t, file)
		sent := sendParts(t, parts, file)
		other := organizerToken(t, e.keys)
		if got := sendJSON(t, e.app, other, fiber.MethodPost, "/v1/uploads/"+id+"/parts", ""); got.status != fiber.StatusNotFound {
			t.Fatalf("another person's parts: status %d body %v", got.status, got.body)
		}
		if got := sendJSON(t, e.app, other, fiber.MethodPost, "/v1/uploads/"+id+"/complete", completeBody(t, sent)); got.status != fiber.StatusNotFound {
			t.Fatalf("another person's completion: status %d body %v", got.status, got.body)
		}
		// Their attempts changed nothing: the uploader completes it.
		done := sendJSON(t, e.app, organizer, fiber.MethodPost, "/v1/uploads/"+id+"/complete", completeBody(t, sent))
		if done.status != fiber.StatusCreated {
			t.Fatalf("the uploader's completion: status %d body %v", done.status, done.body)
		}
		// Nor does its Media answer their retried completion.
		if got := sendJSON(t, e.app, other, fiber.MethodPost, "/v1/uploads/"+id+"/complete", completeBody(t, sent)); got.status != fiber.StatusNotFound {
			t.Fatalf("another person's retried completion: status %d body %v", got.status, got.body)
		}
	})
}

// A Direct upload is charged to the same budget as single-step uploads, for
// the size it declares, when it starts. A file refused at completion is
// given back, and so is an upload that never completes once it expires, or
// one core failed to start.
func TestDirectUploadIsChargedToThePersonsUploadBudgetHTTP(t *testing.T) {
	e := newDirectEnv(t, media.UploadLimits{Count: 3, CountWindow: time.Hour, DailyBytes: 10_000})
	organizer := organizerToken(t, e.keys)
	start := func(size int) jsonResponse {
		return sendJSON(t, e.app, organizer, fiber.MethodPost, "/v1/uploads", startBody("club_file", "a.pdf", size))
	}

	first := start(6000)
	if first.status != fiber.StatusCreated {
		t.Fatalf("start: status %d body %v", first.status, first.body)
	}
	over := start(6000)
	requireCode(t, over, fiber.StatusTooManyRequests, "media_rate_limited")
	if over.body["limit"] != "volume" || over.body["maxDailyBytes"] != float64(10_000) {
		t.Fatalf("problem %v", over.body)
	}

	// The first file is refused at completion (not a PDF): given back.
	file := bytes.Repeat([]byte{'x'}, 6000)
	sent := sendParts(t, partsOf(t, first.body), file)
	refused := sendJSON(t, e.app, organizer, fiber.MethodPost, "/v1/uploads/"+first.body["id"].(string)+"/complete", completeBody(t, sent))
	requireCode(t, refused, fiber.StatusUnsupportedMediaType, "media_type_not_allowed")
	second := start(6000)
	if second.status != fiber.StatusCreated {
		t.Fatalf("after the refusal: status %d body %v", second.status, second.body)
	}

	// The second is never completed: it counts until it expires.
	requireCode(t, start(6000), fiber.StatusTooManyRequests, "media_rate_limited")
	e.clock.Advance(media.DirectUploadTTL)
	third := start(6000)
	if third.status != fiber.StatusCreated {
		t.Fatalf("after the upload expired: status %d body %v", third.status, third.body)
	}
	// A completed upload stays charged.
	done := sendJSON(t, e.app, organizer, fiber.MethodPost, "/v1/uploads/"+third.body["id"].(string)+"/complete",
		completeBody(t, sendParts(t, partsOf(t, third.body), pdfFile(6000))))
	if done.status != fiber.StatusCreated {
		t.Fatalf("complete: status %d body %v", done.status, done.body)
	}
	requireCode(t, start(6000), fiber.StatusTooManyRequests, "media_rate_limited")

	// Storage failing to open the upload is core's failure: given back. With
	// the completed upload, three uploads fit the hour: two more after it.
	e.s3.Fail("CreateMultipartUpload", http.StatusForbidden, "AccessDenied")
	if failed := start(1000); failed.status != fiber.StatusInternalServerError {
		t.Fatalf("storage down: status %d body %v", failed.status, failed.body)
	}
	for i := range 2 {
		if ok := start(1000); ok.status != fiber.StatusCreated {
			t.Fatalf("upload %d after the failure: status %d body %v", i+2, ok.status, ok.body)
		}
	}
	count := start(1000)
	requireCode(t, count, fiber.StatusTooManyRequests, "media_rate_limited")
	if count.body["limit"] != "uploads" {
		t.Fatalf("problem %v", count.body)
	}
}

// An upload never completed is ended by the staging sweeper once it
// expires: its multipart upload is aborted and its record goes.
func TestDirectUploadThatExpiresIsAbortedByTheStagingSweeperHTTP(t *testing.T) {
	e := newDirectEnv(t, media.DefaultUploadLimits())
	organizer := organizerToken(t, e.keys)
	file := pdfFile(partSize + 10)
	started := sendJSON(t, e.app, organizer, fiber.MethodPost, "/v1/uploads", startBody("club_file", "yarım.pdf", len(file)))
	if started.status != fiber.StatusCreated {
		t.Fatalf("start: status %d body %v", started.status, started.body)
	}
	id := started.body["id"].(string)
	sendParts(t, partsOf(t, started.body)[:1], file)

	ctx := context.Background()
	report, err := media.PurgeStagedUploads(ctx, e.store, e.r2, e.clock.Now().Add(time.Hour), 10)
	if err != nil || report.Resolved != 0 {
		t.Fatalf("before its expiry: report %+v err %v", report, err)
	}
	if open := e.s3.OpenUploads("media"); !slices.Equal(open, []string{"pending/" + id}) {
		t.Fatalf("open uploads before expiry %v", open)
	}

	report, err = media.PurgeStagedUploads(ctx, e.store, e.r2, e.clock.Now().Add(media.DirectUploadTTL), 10)
	if err != nil || report.Resolved != 1 {
		t.Fatalf("after its expiry: report %+v err %v", report, err)
	}
	if open, aborted := e.s3.OpenUploads("media"), e.s3.Aborted(); len(open) != 0 || !slices.Equal(aborted, []string{"pending/" + id}) {
		t.Fatalf("open uploads %v, aborted %v", open, aborted)
	}
	if _, err := e.store.GetDirectUpload(ctx, uuid.MustParse(id)); err == nil {
		t.Fatal("the expired upload's record is still there")
	}
	// Running again finds nothing: every step is idempotent.
	if report, err := media.PurgeStagedUploads(ctx, e.store, e.r2, e.clock.Now().Add(media.DirectUploadTTL), 10); err != nil || report.Scanned != 0 {
		t.Fatalf("second pass: report %+v err %v", report, err)
	}
}

// Direct upload checks a purpose as a single-step upload does, in the same
// order, and takes only purposes whose transport is Direct upload.
func TestDirectUploadPurposeRulesHTTP(t *testing.T) {
	t.Parallel()
	keys := testauth.New(t)
	organizer := organizerToken(t, keys)
	member := keys.Token(t, jwt.MapClaims{
		"sub": uuid.NewString(), "email": "member@example.com", "given_name": "M", "family_name": "M",
		"groups": []string{"/UYELER/ARGE/WEBLAB"},
	})
	reviewed := memoryDeps()
	reviewed.ParseToken = keys.Parse()
	reviewedApp := httpx.New(reviewed)

	// The reviewed club_file needs a malware scan, and names no product
	// that attaches it: refused until ticket 12 and a decision.
	requireCode(t, sendJSON(t, reviewedApp, organizer, fiber.MethodPost, "/v1/uploads", startBody("club_file", "a.zip", 100)),
		fiber.StatusUnprocessableEntity, "purpose_not_available")
	requireCode(t, sendJSON(t, reviewedApp, organizer, fiber.MethodPost, "/v1/uploads", startBody("answer_file_large", "a.zip", 100)),
		fiber.StatusForbidden, "purpose_forbidden")
	requireCode(t, sendJSON(t, reviewedApp, organizer, fiber.MethodPost, "/v1/uploads", startBody("event_cover", "a.png", 100)),
		fiber.StatusBadRequest, "purpose_requires_single_step")
	requireCode(t, sendJSON(t, reviewedApp, organizer, fiber.MethodPost, "/v1/uploads", startBody("legacy", "a.pdf", 100)),
		fiber.StatusBadRequest, "purpose_unknown")

	open := memoryDeps()
	open.ParseToken = keys.Parse()
	open.Media = media.NewServiceWithOptions(media.NewMemoryStore(), media.NewMemoryBlob(), authz.NewAuthorizer(authz.DefaultPolicy()), "",
		media.ServiceOptions{Catalogue: directCatalogue(t), ServiceProducts: open.ServiceClients.Products()})
	app := httpx.New(open)
	requireCode(t, sendJSON(t, app, member, fiber.MethodPost, "/v1/uploads", startBody("club_file", "a.pdf", 100)),
		fiber.StatusForbidden, "purpose_forbidden")
	tooLarge := sendJSON(t, app, organizer, fiber.MethodPost, "/v1/uploads", startBody("club_file", "a.pdf", 1<<30+1))
	requireCode(t, tooLarge, fiber.StatusRequestEntityTooLarge, "media_too_large")
	if tooLarge.body["maxBytes"] != float64(1<<30) {
		t.Fatalf("problem %v", tooLarge.body)
	}
	narrowed := sendJSON(t, app, organizer, fiber.MethodPost, "/v1/uploads",
		`{"purpose":"club_file","name":"a.pdf","size":2000,"limits":{"maxBytes":1000}}`)
	requireCode(t, narrowed, fiber.StatusRequestEntityTooLarge, "media_too_large")
	if narrowed.body["maxBytes"] != float64(1000) {
		t.Fatalf("problem %v", narrowed.body)
	}
	for _, limits := range []string{`{"types":["video/mp4"]}`, `{"maxBytes":1073741825}`} {
		wide := sendJSON(t, app, organizer, fiber.MethodPost, "/v1/uploads",
			`{"purpose":"club_file","name":"a.pdf","size":100,"limits":`+limits+`}`)
		requireCode(t, wide, fiber.StatusBadRequest, "media_limits_too_wide")
	}
	for _, body := range []string{startBody("club_file", "", 100), startBody("club_file", "a.pdf", 0), `{"purpose":"club_file","name":"a\u0000.pdf","size":1}`, "not json"} {
		if got := sendJSON(t, app, organizer, fiber.MethodPost, "/v1/uploads", body); got.status != fiber.StatusBadRequest {
			t.Fatalf("%s: status %d body %v", body, got.status, got.body)
		}
	}
	// This core has no R2: a start that passes every rule has nowhere to go.
	requireCode(t, sendJSON(t, app, organizer, fiber.MethodPost, "/v1/uploads", startBody("club_file", "a.pdf", 100)),
		fiber.StatusServiceUnavailable, "direct_upload_unavailable")
	if anonymous := sendJSON(t, app, "", fiber.MethodPost, "/v1/uploads", startBody("club_file", "a.pdf", 100)); anonymous.status != fiber.StatusUnauthorized {
		t.Fatalf("anonymous: status %d", anonymous.status)
	}
}
