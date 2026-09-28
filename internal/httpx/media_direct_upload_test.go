package httpx_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
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
// against a fake S3, and the Direct upload budget on a clock the test moves.
type directEnv struct {
	app     *fiber.App
	pool    *pgxpool.Pool
	keys    *testauth.Bundle
	s3      *s3test.Server
	r2      *media.R2
	store   *media.PostgresStore
	clock   *manualClock
	limiter *media.DirectUploadLimiter
	// singleStep is the single-step upload budget: Direct upload never
	// touches it (decision Q23).
	singleStep *media.UploadLimiter
}

func newDirectEnv(t *testing.T, limits media.DirectUploadLimits) *directEnv {
	t.Helper()
	return newDirectEnvWithPool(t, limits, 0)
}

// newDirectEnvWithPool is newDirectEnv whose core has at most maxConns
// database connections (0: pgxpool's default).
func newDirectEnvWithPool(t *testing.T, limits media.DirectUploadLimits, maxConns int32) *directEnv {
	t.Helper()
	pool := testpostgres.Start(t)
	if err := migrate.Apply(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	if maxConns > 0 {
		config := pool.Config().Copy()
		config.MaxConns = maxConns
		small, err := pgxpool.NewWithConfig(context.Background(), config)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(small.Close)
		pool = small
	}
	fake := s3test.New(t)
	clock := &manualClock{now: time.Now().UTC().Truncate(time.Second)}
	e := &directEnv{
		pool: pool, keys: testauth.New(t), s3: fake,
		r2:         media.NewR2(media.R2Config{Endpoint: fake.URL, AccessKey: "access", SecretKey: "secret", Bucket: "media"}),
		store:      media.NewPostgresStore(pool),
		clock:      clock,
		limiter:    media.NewDirectUploadLimiter(limits, clock.Now),
		singleStep: media.NewUploadLimiter(media.DefaultUploadLimits(), clock.Now),
	}
	e.app = e.appWith(t, directCatalogue(t))
	return e
}

// appWith is another core over the same database, storage and budget, with
// its own catalogue: the same core after a deploy that changed it.
func (e *directEnv) appWith(t *testing.T, catalogue media.Catalogue) *fiber.App {
	t.Helper()
	deps := memoryDeps()
	deps.Users = user.NewService(user.NewPostgresStore(e.pool))
	deps.ParseToken = e.keys.Parse()
	deps.MediaUploadLimiter = e.singleStep
	deps.Media = media.NewServiceWithOptions(e.store, e.r2, authz.NewAuthorizer(authz.DefaultPolicy()), "https://cdn.example.test",
		media.ServiceOptions{
			Catalogue:       catalogue,
			ServiceProducts: deps.ServiceClients.Products(),
			Direct:          media.DirectUploadConfig{Storage: e.r2, Limiter: e.limiter, Now: e.clock.Now},
		})
	return httpx.New(deps)
}

// catalogueWith is directCatalogue after one change to a purpose.
func catalogueWith(t *testing.T, purpose string, change func(entry map[string]any)) media.Catalogue {
	t.Helper()
	var file map[string]any
	if err := json.Unmarshal(config.MediaPurposes, &file); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"club_file", "video"} {
		entry := file["purposes"].(map[string]any)[name].(map[string]any)
		entry["scan"] = false
		entry["attach"] = "core"
	}
	change(file["purposes"].(map[string]any)[purpose].(map[string]any))
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

// organizerToken is an organizer's token: someone who may create Events,
// so the club_file purpose's event_editor rule lets them upload.
func organizerToken(t *testing.T, keys *testauth.Bundle) string {
	t.Helper()
	_, token := newOrganizer(t, keys)
	return token
}

// newOrganizer is an organizer's id and token.
func newOrganizer(t *testing.T, keys *testauth.Bundle) (uuid.UUID, string) {
	t.Helper()
	id := uuid.New()
	return id, keys.Token(t, jwt.MapClaims{
		"sub": id.String(), "email": uuid.NewString() + "@example.com", "given_name": "Y", "family_name": "K",
		"groups": []string{"/UYELER/YK"},
	})
}

// startSent starts an upload of file and sends its parts: the completion
// body is ready.
func (e *directEnv) startSent(t *testing.T, token, name string, file []byte) (string, string) {
	t.Helper()
	started := sendJSON(t, e.app, token, fiber.MethodPost, "/v1/uploads", startBody("club_file", name, len(file)))
	if started.status != fiber.StatusCreated {
		t.Fatalf("start: status %d body %v", started.status, started.body)
	}
	return started.body["id"].(string), completeBody(t, sendParts(t, partsOf(t, started.body), file))
}

// completeAsync completes the upload in the background, waiting as long as
// storage is held.
func (e *directEnv) completeAsync(t *testing.T, token, id, body string) <-chan jsonResponse {
	t.Helper()
	answer := make(chan jsonResponse, 1)
	go func() {
		req := httptest.NewRequest(fiber.MethodPost, "/v1/uploads/"+id+"/complete", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := e.app.Test(req, fiber.TestConfig{Timeout: 30 * time.Second})
		if err != nil {
			answer <- jsonResponse{status: -1, body: map[string]any{"error": err.Error()}}
			return
		}
		defer resp.Body.Close()
		got := map[string]any{}
		_ = json.NewDecoder(resp.Body).Decode(&got)
		answer <- jsonResponse{status: resp.StatusCode, body: got}
	}()
	return answer
}

// waitEntered waits until n requests are held.
func waitEntered(t *testing.T, entered <-chan struct{}, n int) {
	t.Helper()
	for i := range n {
		select {
		case <-entered:
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d of %d requests reached storage", i, n)
		}
	}
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
	e := newDirectEnv(t, media.DefaultDirectUploadLimits())
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
		// Should the pending object ever be reached at its CDN address, it
		// downloads; it never renders.
		if meta, ok := e.s3.UploadMetadata("media", "pending/"+id); !ok || meta.ContentType != "application/octet-stream" ||
			meta.ContentDisposition != "attachment" {
			t.Fatalf("pending upload metadata %+v (found %v)", meta, ok)
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
	e := newDirectEnv(t, media.DefaultDirectUploadLimits())
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

	t.Run("the catalogue as it is at completion", func(t *testing.T) {
		// A ZIP started while club_file took ZIPs, completed after a deploy
		// that took ZIP out of it.
		file := zipFile(t, 100)
		started := sendJSON(t, e.app, organizer, fiber.MethodPost, "/v1/uploads", startBody("club_file", "a.zip", len(file)))
		if started.status != fiber.StatusCreated {
			t.Fatalf("start: status %d body %v", started.status, started.body)
		}
		id := started.body["id"].(string)
		sent := sendParts(t, partsOf(t, started.body), file)
		deployed := e.appWith(t, catalogueWith(t, "club_file", func(entry map[string]any) {
			entry["types"] = []any{"application/pdf"}
		}))
		done := sendJSON(t, deployed, organizer, fiber.MethodPost, "/v1/uploads/"+id+"/complete", completeBody(t, sent))
		requireCode(t, done, fiber.StatusUnsupportedMediaType, "media_type_not_allowed")
		if allowed, _ := done.body["allowedTypes"].([]any); len(allowed) != 1 || allowed[0] != "application/pdf" {
			t.Fatalf("problem %v", done.body)
		}
		ended(t, id, sent)

		// And a maximum the deploy lowered under the file.
		file = pdfFile(1<<20 + 10)
		started = sendJSON(t, e.app, organizer, fiber.MethodPost, "/v1/uploads", startBody("club_file", "a.pdf", len(file)))
		if started.status != fiber.StatusCreated {
			t.Fatalf("start: status %d body %v", started.status, started.body)
		}
		id = started.body["id"].(string)
		sent = sendParts(t, partsOf(t, started.body), file)
		smaller := e.appWith(t, catalogueWith(t, "club_file", func(entry map[string]any) { entry["max_mib"] = 1 }))
		done = sendJSON(t, smaller, organizer, fiber.MethodPost, "/v1/uploads/"+id+"/complete", completeBody(t, sent))
		requireCode(t, done, fiber.StatusRequestEntityTooLarge, "media_too_large")
		if done.body["maxBytes"] != float64(1<<20) {
			t.Fatalf("problem %v", done.body)
		}
		ended(t, id, sent)
	})

	t.Run("a completion core fails leaves no pending object", func(t *testing.T) {
		file := pdfFile(3000)
		id, parts := start(t, file)
		sent := sendParts(t, parts, file)
		e.s3.Fail("GetObject", http.StatusForbidden, "AccessDenied")
		done := sendJSON(t, e.app, organizer, fiber.MethodPost, "/v1/uploads/"+id+"/complete", completeBody(t, sent))
		if done.status != fiber.StatusInternalServerError {
			t.Fatalf("complete: status %d body %v", done.status, done.body)
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

// Direct upload has its own budget, apart from single-step uploads
// (decision Q23: the product that grants a Direct upload limits it, and
// core is that product for the uploads it starts): a few uploads open at
// once, and a volume of declared bytes per rolling day. A file refused at
// completion stays charged, as a single-step refusal does, so refused 2 GiB
// uploads cannot be repeated without end; only core's own failures are
// given back.
func TestDirectUploadHasItsOwnBudgetHTTP(t *testing.T) {
	e := newDirectEnv(t, media.DirectUploadLimits{MaxOpen: 2, DailyBytes: 10_000})

	t.Run("a refused file stays charged", func(t *testing.T) {
		organizer := organizerToken(t, e.keys)
		first := sendJSON(t, e.app, organizer, fiber.MethodPost, "/v1/uploads", startBody("club_file", "a.pdf", 6000))
		if first.status != fiber.StatusCreated {
			t.Fatalf("start: status %d body %v", first.status, first.body)
		}
		sent := sendParts(t, partsOf(t, first.body), bytes.Repeat([]byte{'x'}, 6000))
		refused := sendJSON(t, e.app, organizer, fiber.MethodPost, "/v1/uploads/"+first.body["id"].(string)+"/complete", completeBody(t, sent))
		requireCode(t, refused, fiber.StatusUnsupportedMediaType, "media_type_not_allowed")

		again := sendJSON(t, e.app, organizer, fiber.MethodPost, "/v1/uploads", startBody("club_file", "a.pdf", 6000))
		requireCode(t, again, fiber.StatusTooManyRequests, "media_rate_limited")
		if again.body["limit"] != "volume" || again.body["maxDailyBytes"] != float64(10_000) || again.body["retryAfterSeconds"] == nil {
			t.Fatalf("problem %v", again.body)
		}
		// The next day it fits again.
		e.clock.Advance(24 * time.Hour)
		if next := sendJSON(t, e.app, organizer, fiber.MethodPost, "/v1/uploads", startBody("club_file", "a.pdf", 6000)); next.status != fiber.StatusCreated {
			t.Fatalf("a day later: status %d body %v", next.status, next.body)
		}
	})

	t.Run("a few uploads open at once", func(t *testing.T) {
		organizer := organizerToken(t, e.keys)
		var open []jsonResponse
		for range 2 {
			started := sendJSON(t, e.app, organizer, fiber.MethodPost, "/v1/uploads", startBody("club_file", "a.pdf", 1000))
			if started.status != fiber.StatusCreated {
				t.Fatalf("start: status %d body %v", started.status, started.body)
			}
			open = append(open, started)
		}
		third := sendJSON(t, e.app, organizer, fiber.MethodPost, "/v1/uploads", startBody("club_file", "a.pdf", 1000))
		requireCode(t, third, fiber.StatusTooManyRequests, "media_rate_limited")
		if third.body["limit"] != "open" || third.body["maxOpenUploads"] != float64(2) ||
			third.body["retryAfterSeconds"] != float64(media.DirectUploadTTL/time.Second) {
			t.Fatalf("problem %v", third.body)
		}
		// Completing one frees its place.
		done := sendJSON(t, e.app, organizer, fiber.MethodPost, "/v1/uploads/"+open[0].body["id"].(string)+"/complete",
			completeBody(t, sendParts(t, partsOf(t, open[0].body), pdfFile(1000))))
		if done.status != fiber.StatusCreated {
			t.Fatalf("complete: status %d body %v", done.status, done.body)
		}
		if started := sendJSON(t, e.app, organizer, fiber.MethodPost, "/v1/uploads", startBody("club_file", "a.pdf", 1000)); started.status != fiber.StatusCreated {
			t.Fatalf("after a completion: status %d body %v", started.status, started.body)
		}
		// So does an upload expiring, swept or not.
		requireCode(t, sendJSON(t, e.app, organizer, fiber.MethodPost, "/v1/uploads", startBody("club_file", "a.pdf", 1000)),
			fiber.StatusTooManyRequests, "media_rate_limited")
		e.clock.Advance(media.DirectUploadTTL)
		if started := sendJSON(t, e.app, organizer, fiber.MethodPost, "/v1/uploads", startBody("club_file", "a.pdf", 1000)); started.status != fiber.StatusCreated {
			t.Fatalf("after the uploads expired: status %d body %v", started.status, started.body)
		}
	})

	t.Run("core's own failures are given back", func(t *testing.T) {
		organizer := organizerToken(t, e.keys)
		e.s3.Fail("CreateMultipartUpload", http.StatusForbidden, "AccessDenied")
		if failed := sendJSON(t, e.app, organizer, fiber.MethodPost, "/v1/uploads", startBody("club_file", "a.pdf", 6000)); failed.status != fiber.StatusInternalServerError {
			t.Fatalf("storage down at the start: status %d body %v", failed.status, failed.body)
		}
		started := sendJSON(t, e.app, organizer, fiber.MethodPost, "/v1/uploads", startBody("club_file", "a.pdf", 6000))
		if started.status != fiber.StatusCreated {
			t.Fatalf("after the failed start: status %d body %v", started.status, started.body)
		}
		id := started.body["id"].(string)
		sent := sendParts(t, partsOf(t, started.body), pdfFile(6000))
		e.s3.Fail("CopyObject", http.StatusForbidden, "AccessDenied")
		failed := sendJSON(t, e.app, organizer, fiber.MethodPost, "/v1/uploads/"+id+"/complete", completeBody(t, sent))
		if failed.status != fiber.StatusInternalServerError {
			t.Fatalf("storage down at the copy: status %d body %v", failed.status, failed.body)
		}
		// The joined object is not left at its pending key, and the upload
		// is over: the file must be sent again.
		if _, pending := e.s3.Object("media", "pending/"+id); pending {
			t.Fatal("the failed completion left its pending object")
		}
		if again := sendJSON(t, e.app, organizer, fiber.MethodPost, "/v1/uploads/"+id+"/complete", completeBody(t, sent)); again.status != fiber.StatusNotFound {
			t.Fatalf("completing it again: status %d body %v", again.status, again.body)
		}
		if next := sendJSON(t, e.app, organizer, fiber.MethodPost, "/v1/uploads", startBody("club_file", "a.pdf", 6000)); next.status != fiber.StatusCreated {
			t.Fatalf("after the failed completion: status %d body %v", next.status, next.body)
		}
	})

	if tracked := e.singleStep.Tracked(); tracked != 0 {
		t.Fatalf("Direct uploads were charged to the single-step budget (%d people)", tracked)
	}
}

// A completion holds no database lock or connection while storage works:
// with a pool of two connections, three completions copying at once all
// finish, and the database answers other requests meanwhile.
func TestDirectUploadCompletionsHoldNoConnectionDuringTheCopyHTTP(t *testing.T) {
	e := newDirectEnvWithPool(t, media.DefaultDirectUploadLimits(), 2)
	organizer := organizerToken(t, e.keys)
	type upload struct{ id, body string }
	var uploads []upload
	for range 3 {
		id, body := e.startSent(t, organizer, "üç kopya.pdf", pdfFile(3000))
		uploads = append(uploads, upload{id, body})
	}
	entered, release := e.s3.Hold("CopyObject")
	var answers []<-chan jsonResponse
	for _, u := range uploads {
		answers = append(answers, e.completeAsync(t, organizer, u.id, u.body))
	}
	waitEntered(t, entered, 3)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := e.store.GetDirectUpload(ctx, uuid.MustParse(uploads[0].id)); err != nil {
		t.Fatalf("the database during the copies: %v", err)
	}
	if got := sendJSON(t, e.app, organizer, fiber.MethodPost, "/v1/uploads", startBody("club_file", "a.pdf", 100)); got.status != fiber.StatusTooManyRequests {
		t.Fatalf("a start during the copies: status %d body %v", got.status, got.body)
	}
	release()
	for i, answer := range answers {
		if done := <-answer; done.status != fiber.StatusCreated || done.body["id"] != uploads[i].id {
			t.Fatalf("complete %d: status %d body %v", i, done.status, done.body)
		}
	}
}

// A second completion of an upload being completed (a retry racing the
// first) does not wait on the first: it is told to retry, and the retry
// answers the Media the first created. The file is copied once.
func TestDirectUploadCompletionOfAnUploadBeingCompletedIsToldToRetryHTTP(t *testing.T) {
	e := newDirectEnv(t, media.DefaultDirectUploadLimits())
	organizer := organizerToken(t, e.keys)
	id, body := e.startSent(t, organizer, "iki kez.pdf", pdfFile(partSize+99))
	entered, release := e.s3.Hold("CopyObject")
	first := e.completeAsync(t, organizer, id, body)
	waitEntered(t, entered, 1)

	second := sendJSON(t, e.app, organizer, fiber.MethodPost, "/v1/uploads/"+id+"/complete", body)
	requireCode(t, second, fiber.StatusConflict, "upload_completing")
	if second.body["retryAfterSeconds"] == nil {
		t.Fatalf("problem %v", second.body)
	}
	if parts := sendJSON(t, e.app, organizer, fiber.MethodPost, "/v1/uploads/"+id+"/parts", ""); parts.status != fiber.StatusConflict {
		t.Fatalf("parts while completing: status %d body %v", parts.status, parts.body)
	}
	release()
	done := <-first
	if done.status != fiber.StatusCreated || done.body["id"] != id {
		t.Fatalf("complete: status %d body %v", done.status, done.body)
	}
	retried := sendJSON(t, e.app, organizer, fiber.MethodPost, "/v1/uploads/"+id+"/complete", body)
	if retried.status != fiber.StatusCreated || retried.body["id"] != id || retried.body["url"] != done.body["url"] {
		t.Fatalf("retry: status %d body %v", retried.status, retried.body)
	}
	if copies := e.s3.Count("CopyObject"); copies != 1 {
		t.Fatalf("the file was copied %d times", copies)
	}
}

// Account erasure's staged-upload step meets an upload being completed as a
// live upload: it defers (its attempt is given back) at once, and does not
// wait for the copy.
func TestDirectUploadBeingCompletedDefersAccountErasureHTTP(t *testing.T) {
	e := newDirectEnv(t, media.DefaultDirectUploadLimits())
	uploader, organizer := newOrganizer(t, e.keys)
	id, body := e.startSent(t, organizer, "silinecek.pdf", pdfFile(3000))
	entered, release := e.s3.Hold("CopyObject")
	answer := e.completeAsync(t, organizer, id, body)
	waitEntered(t, entered, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	err := media.NewImmediateBlobEraser(e.store, media.Buckets{Public: e.r2}).EnsureSubjectUploadsErased(ctx, uploader, e.clock.Now())
	var deferred interface{ RetryAt() time.Time }
	if !errors.As(err, &deferred) || errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatalf("erasure during the copy: %v after %s", err, time.Since(start))
	}
	if !deferred.RetryAt().After(e.clock.Now()) {
		t.Fatalf("retry at %s", deferred.RetryAt())
	}
	release()
	if done := <-answer; done.status != fiber.StatusCreated {
		t.Fatalf("complete: status %d body %v", done.status, done.body)
	}
}

// A completion whose Media cannot be stored ends the upload: the joined file
// is deleted with its copy, the upload no longer counts as open, and core's
// failure is given back. An account being erased meanwhile is a refusal.
func TestDirectUploadThatCannotBeStoredEndsHTTP(t *testing.T) {
	e := newDirectEnv(t, media.DirectUploadLimits{MaxOpen: 1, DailyBytes: 10_000})

	t.Run("core fails to store it", func(t *testing.T) {
		organizer := organizerToken(t, e.keys)
		id, body := e.startSent(t, organizer, "kayıp.pdf", pdfFile(6000))
		entered, release := e.s3.Hold("CopyObject")
		answer := e.completeAsync(t, organizer, id, body)
		waitEntered(t, entered, 1)
		// The final key's staging row goes: storing the Media fails.
		if _, err := e.pool.Exec(context.Background(), `DELETE FROM media_upload_staging WHERE object_key LIKE 'files/%'`); err != nil {
			t.Fatal(err)
		}
		release()
		if done := <-answer; done.status != fiber.StatusInternalServerError {
			t.Fatalf("complete: status %d body %v", done.status, done.body)
		}
		for _, key := range e.s3.Keys("media") {
			if strings.HasPrefix(key, "pending/") || strings.HasPrefix(key, "files/") {
				t.Fatalf("left in storage: %s", key)
			}
		}
		if _, err := e.store.GetDirectUpload(context.Background(), uuid.MustParse(id)); !errors.Is(err, media.ErrNotFound) {
			t.Fatalf("the upload is still there: %v", err)
		}
		// Its place and its 6000 bytes are free again.
		if next := sendJSON(t, e.app, organizer, fiber.MethodPost, "/v1/uploads", startBody("club_file", "a.pdf", 6000)); next.status != fiber.StatusCreated {
			t.Fatalf("after the failure: status %d body %v", next.status, next.body)
		}
	})

	t.Run("the uploader's account is being erased", func(t *testing.T) {
		uploader, organizer := newOrganizer(t, e.keys)
		id, body := e.startSent(t, organizer, "gidecek.pdf", pdfFile(3000))
		entered, release := e.s3.Hold("CopyObject")
		answer := e.completeAsync(t, organizer, id, body)
		waitEntered(t, entered, 1)
		if _, err := user.NewPostgresStore(e.pool).RequestDeletion(context.Background(), uploader, nil); err != nil {
			t.Fatal(err)
		}
		release()
		if done := <-answer; done.status != fiber.StatusForbidden {
			t.Fatalf("complete: status %d body %v", done.status, done.body)
		}
		if _, pending := e.s3.Object("media", "pending/"+id); pending {
			t.Fatal("the refused completion left its pending object")
		}
	})
}

// An upload never completed is ended by the staging sweeper once it
// expires: its multipart upload is aborted and its record goes.
func TestDirectUploadThatExpiresIsAbortedByTheStagingSweeperHTTP(t *testing.T) {
	e := newDirectEnv(t, media.DefaultDirectUploadLimits())
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
	for _, body := range []string{
		startBody("club_file", "", 100), startBody("club_file", "a.pdf", 0), `{"purpose":"club_file","name":"a\u0000.pdf","size":1}`,
		// A right-to-left override shows "a\u202Epiz.exe" as "aexe.zip".
		`{"purpose":"club_file","name":"a\u202epiz.exe","size":1}`, `{"purpose":"club_file","name":"a\u2066b.pdf","size":1}`,
		"not json",
	} {
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

// Every upload, single-step, without a purpose or as a profile picture,
// refuses a file name with a control or an invisible format character: a
// right-to-left override shows "a‮gnp.exe" as "aexe.png".
func TestUploadsRefuseFileNamesThatReadAsAnotherHTTP(t *testing.T) {
	t.Parallel()
	keys := testauth.New(t)
	deps := memoryDeps()
	deps.ParseToken = keys.Parse()
	app := httpx.New(deps)
	organizer := organizerToken(t, keys)
	post := func(path, purpose, name string) int {
		t.Helper()
		var body bytes.Buffer
		form := multipart.NewWriter(&body)
		if purpose != "" {
			if err := form.WriteField("purpose", purpose); err != nil {
				t.Fatal(err)
			}
		}
		part, err := form.CreateFormFile("file", name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(pngPicture(t)); err != nil {
			t.Fatal(err)
		}
		if err := form.Close(); err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(fiber.MethodPost, path, &body)
		req.Header.Set("Content-Type", form.FormDataContentType())
		req.Header.Set("Authorization", "Bearer "+organizer)
		resp, err := app.Test(req, fiber.TestConfig{Timeout: 10 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	for _, name := range []string{"a‮gnp.exe", "kapak⁦.png", "a​.png"} {
		for _, upload := range []struct{ path, purpose string }{
			{"/v1/media", "event_cover"}, {"/v1/media", ""}, {"/v1/users/me/profile-picture", ""},
		} {
			if status := post(upload.path, upload.purpose, name); status != fiber.StatusBadRequest {
				t.Errorf("%s (%q) named %q: status %d", upload.path, upload.purpose, name, status)
			}
		}
	}
	if status := post("/v1/media", "event_cover", "kapak görseli.png"); status != fiber.StatusCreated {
		t.Fatalf("a plain name: status %d", status)
	}
}
