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
	"sync"
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
	"github.com/skylab-kulubu/core-backend/internal/transit"
	"github.com/skylab-kulubu/core-backend/internal/transit/transittest"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// partSize is the part size core hands out.
const partSize = 16 << 20

// directCatalogue is the reviewed catalogue with club_file's malware scan
// off: these tests run without a scanner, where the reviewed club_file is
// refused (media_scan_test.go has one). Core attaches club files and videos
// (an Event's files and videos) in the reviewed catalogue already.
func directCatalogue(t testing.TB) media.Catalogue {
	t.Helper()
	var file map[string]any
	if err := json.Unmarshal(config.MediaPurposes, &file); err != nil {
		t.Fatal(err)
	}
	file["purposes"].(map[string]any)["club_file"].(map[string]any)["scan"] = false
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

// openDirectPurposes are the Direct upload purposes these tests' cores
// switch on (MEDIA_DIRECT_UPLOAD_PURPOSES): every one core attaches.
var openDirectPurposes = []string{"club_file", "video"}

// appWith is another core over the same database, storage and budget, with
// its own catalogue: the same core after a deploy that changed it.
func (e *directEnv) appWith(t *testing.T, catalogue media.Catalogue) *fiber.App {
	t.Helper()
	return e.appWithPurposes(t, catalogue, openDirectPurposes...)
}

// appWithPurposes is appWith with only the Direct upload purposes named
// switched on.
func (e *directEnv) appWithPurposes(t *testing.T, catalogue media.Catalogue, purposes ...string) *fiber.App {
	t.Helper()
	deps := memoryDeps()
	deps.Users = user.NewService(user.NewPostgresStore(e.pool))
	deps.ParseToken = e.keys.Parse()
	deps.MediaUploadLimiter = e.singleStep
	deps.Media = media.NewServiceWithOptions(e.store, e.r2, authz.NewAuthorizer(authz.DefaultPolicy()), "https://cdn.example.test",
		media.ServiceOptions{
			Catalogue:       catalogue,
			ServiceProducts: deps.ServiceClients.Products(),
			Direct:          media.DirectUploadConfig{Storage: e.r2, Limiter: e.limiter, Now: e.clock.Now, Purposes: purposes},
		})
	return httpx.New(deps)
}

// hookedStorage is R2 with hooks around the copy and a switch that fails
// deletes: a storage slower or less reliable than the fake.
type hookedStorage struct {
	*media.R2
	mu         sync.Mutex
	copies     int
	beforeCopy func(n int, to string)
	// copyResult, when it returns skip, answers the copy instead of R2.
	copyResult func(n int, to string) (skip bool, err error)
	// afterCopy runs once R2 copied.
	afterCopy  func(n int, to string)
	failDelete func(key string) bool
}

func (h *hookedStorage) Copy(ctx context.Context, from, to string, meta media.BlobMetadata) error {
	h.mu.Lock()
	h.copies++
	n, before, result, after := h.copies, h.beforeCopy, h.copyResult, h.afterCopy
	h.mu.Unlock()
	if before != nil {
		before(n, to)
	}
	if result != nil {
		if skip, err := result(n, to); skip {
			return err
		}
	}
	if err := h.R2.Copy(ctx, from, to, meta); err != nil {
		return err
	}
	if after != nil {
		after(n, to)
	}
	return nil
}

func (h *hookedStorage) Delete(ctx context.Context, key string) error {
	h.mu.Lock()
	fail := h.failDelete
	h.mu.Unlock()
	if fail != nil && fail(key) {
		return errors.New("storage: delete failed")
	}
	return h.R2.Delete(ctx, key)
}

// withStorage serves e's core with Direct upload's storage replaced.
func (e *directEnv) withStorage(t *testing.T, storage media.MultipartStore) *fiber.App {
	t.Helper()
	deps := memoryDeps()
	deps.Users = user.NewService(user.NewPostgresStore(e.pool))
	deps.ParseToken = e.keys.Parse()
	deps.MediaUploadLimiter = e.singleStep
	deps.Media = media.NewServiceWithOptions(e.store, e.r2, authz.NewAuthorizer(authz.DefaultPolicy()), "https://cdn.example.test",
		media.ServiceOptions{
			Catalogue:       directCatalogue(t),
			ServiceProducts: deps.ServiceClients.Products(),
			Direct:          media.DirectUploadConfig{Storage: storage, Limiter: e.limiter, Now: e.clock.Now, Purposes: openDirectPurposes},
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
	file["purposes"].(map[string]any)["club_file"].(map[string]any)["scan"] = false
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

// A copy whose outcome core does not know (it timed out, or failed) may
// still land at its final key after core gave up. That key stays staged
// until well after the lease, so the sweeper, or the uploader's account
// erasure, deletes the late copy.
func TestDirectUploadCopyThatLandsLateIsStillDeletedHTTP(t *testing.T) {
	e := newDirectEnv(t, media.DefaultDirectUploadLimits())
	hook := &hookedStorage{R2: e.r2}
	app := e.withStorage(t, hook)
	late := func(t *testing.T, organizer string) (finalKey string, claimedAt time.Time) {
		t.Helper()
		id, body := e.startSent(t, organizer, "geç.pdf", pdfFile(3000))
		claimedAt = e.clock.Now()
		hook.copyResult = func(_ int, to string) (bool, error) {
			finalKey = to
			return true, context.DeadlineExceeded
		}
		done := sendJSON(t, app, organizer, fiber.MethodPost, "/v1/uploads/"+id+"/complete", body)
		if done.status != fiber.StatusInternalServerError {
			t.Fatalf("complete: status %d body %v", done.status, done.body)
		}
		hook.copyResult = nil
		// R2 finishes the copy after core gave up on it.
		if err := e.r2.Put(context.Background(), finalKey, pdfFile(3000), media.BlobMetadata{ContentType: "application/pdf"}); err != nil {
			t.Fatal(err)
		}
		return finalKey, claimedAt
	}
	due := func(claimedAt time.Time) time.Time {
		return claimedAt.Add(media.DirectUploadClaimLease + media.DirectUploadLateCopyMargin)
	}

	t.Run("the sweeper", func(t *testing.T) {
		finalKey, claimedAt := late(t, organizerToken(t, e.keys))
		if _, err := media.PurgeStagedUploads(context.Background(), e.store, e.r2, e.clock.Now().Add(media.DirectUploadClaimLease), 10); err != nil {
			t.Fatal(err)
		}
		if _, ok := e.s3.Object("media", finalKey); !ok {
			t.Fatal("the sweeper deleted the copy before a late copy could have landed")
		}
		if _, err := media.PurgeStagedUploads(context.Background(), e.store, e.r2, due(claimedAt), 10); err != nil {
			t.Fatal(err)
		}
		if _, ok := e.s3.Object("media", finalKey); ok {
			t.Fatal("the late copy survived the sweeper")
		}
	})

	t.Run("account erasure", func(t *testing.T) {
		uploader, organizer := newOrganizer(t, e.keys)
		finalKey, claimedAt := late(t, organizer)
		if _, err := user.NewPostgresStore(e.pool).RequestDeletion(context.Background(), uploader, nil); err != nil {
			t.Fatal(err)
		}
		eraser := media.NewImmediateBlobEraser(e.store, media.Buckets{Public: e.r2})
		var deferred interface{ RetryAt() time.Time }
		if err := eraser.EnsureSubjectUploadsErased(context.Background(), uploader, e.clock.Now()); !errors.As(err, &deferred) {
			t.Fatalf("erasure while a late copy may land: %v", err)
		}
		if err := eraser.EnsureSubjectUploadsErased(context.Background(), uploader, due(claimedAt)); err != nil {
			t.Fatal(err)
		}
		if _, ok := e.s3.Object("media", finalKey); ok {
			t.Fatal("the late copy survived the erasure")
		}
	})
}

// A completion whose lease ran out before it finished answers a retryable
// upload_claim_lost and gives the volume back. Its copy is its own to
// delete; when that delete fails, the copy stays staged, and the sweeper or
// the uploader's account erasure deletes it.
func TestDirectUploadLostClaimsCopyIsStillDeletedHTTP(t *testing.T) {
	e := newDirectEnv(t, media.DirectUploadLimits{MaxOpen: 3, DailyBytes: 10_000})
	hook := &hookedStorage{R2: e.r2}
	app := e.withStorage(t, hook)
	lost := func(t *testing.T, organizer string) string {
		t.Helper()
		id, body := e.startSent(t, organizer, "yavaş.pdf", pdfFile(6000))
		var finalKey string
		hook.afterCopy = func(_ int, to string) {
			finalKey = to
			// The copy outlives the lease, and the sweeper ends the upload.
			e.clock.Advance(media.DirectUploadClaimLease + time.Minute)
			if _, err := media.PurgeStagedUploads(context.Background(), e.store, e.r2, e.clock.Now(), 10); err != nil {
				t.Error(err)
			}
		}
		hook.failDelete = func(key string) bool { return strings.HasPrefix(key, "files/") }
		done := sendJSON(t, app, organizer, fiber.MethodPost, "/v1/uploads/"+id+"/complete", body)
		hook.afterCopy, hook.failDelete = nil, nil
		requireCode(t, done, fiber.StatusServiceUnavailable, "upload_claim_lost")
		if _, ok := e.s3.Object("media", finalKey); !ok {
			t.Fatal("the copy is not there: the test did not fail its delete")
		}
		// Core's failure: the 6000 bytes are given back.
		if again := sendJSON(t, app, organizer, fiber.MethodPost, "/v1/uploads", startBody("club_file", "a.pdf", 6000)); again.status != fiber.StatusCreated {
			t.Fatalf("after the lost claim: status %d body %v", again.status, again.body)
		}
		return finalKey
	}

	t.Run("the sweeper", func(t *testing.T) {
		finalKey := lost(t, organizerToken(t, e.keys))
		if _, err := media.PurgeStagedUploads(context.Background(), e.store, e.r2, e.clock.Now(), 10); err != nil {
			t.Fatal(err)
		}
		if _, ok := e.s3.Object("media", finalKey); ok {
			t.Fatal("the lost claim's copy survived the sweeper")
		}
	})

	t.Run("account erasure", func(t *testing.T) {
		uploader, organizer := newOrganizer(t, e.keys)
		finalKey := lost(t, organizer)
		if _, err := user.NewPostgresStore(e.pool).RequestDeletion(context.Background(), uploader, nil); err != nil {
			t.Fatal(err)
		}
		// The new upload the test started is still open: erasure waits for
		// it, and takes the copy on its way.
		if err := media.NewImmediateBlobEraser(e.store, media.Buckets{Public: e.r2}).EnsureSubjectUploadsErased(
			context.Background(), uploader, e.clock.Now().Add(media.DirectUploadTTL)); err != nil {
			t.Fatal(err)
		}
		if _, ok := e.s3.Object("media", finalKey); ok {
			t.Fatal("the lost claim's copy survived the erasure")
		}
	})
}

// A completion that fails after its lease ran out, while another completion
// has claimed the upload again and is copying it, deletes nothing of the
// upload: it is no longer its own. The other completion creates the Media.
func TestDirectUploadStaleCompletionLeavesTheNewClaimAloneHTTP(t *testing.T) {
	e := newDirectEnv(t, media.DefaultDirectUploadLimits())
	hook := &hookedStorage{R2: e.r2}
	app := e.withStorage(t, hook)
	e.app = app
	organizer := organizerToken(t, e.keys)
	id, body := e.startSent(t, organizer, "yeniden.pdf", pdfFile(3000))
	secondCopying := make(chan struct{})
	firstDone := make(chan struct{})
	var second <-chan jsonResponse
	hook.beforeCopy = func(n int, _ string) {
		if n == 2 {
			close(secondCopying)
			<-firstDone
		}
	}
	hook.copyResult = func(n int, _ string) (bool, error) {
		if n != 1 {
			return false, nil
		}
		// The first completion's lease runs out mid-copy; a retry claims
		// the upload again and starts its own copy; then the first copy
		// fails.
		e.clock.Advance(media.DirectUploadClaimLease + time.Second)
		second = e.completeAsync(t, organizer, id, body)
		<-secondCopying
		return true, errors.New("storage: the copy failed")
	}
	first := <-e.completeAsync(t, organizer, id, body)
	close(firstDone)
	requireCode(t, first, fiber.StatusServiceUnavailable, "upload_claim_lost")
	done := <-second
	if done.status != fiber.StatusCreated || done.body["id"] != id {
		t.Fatalf("the second completion: status %d body %v", done.status, done.body)
	}
	key := strings.TrimPrefix(done.body["url"].(string), "https://cdn.example.test/")
	if _, ok := e.s3.Object("media", key); !ok {
		t.Fatal("the Media's object is missing")
	}
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

	// The reviewed club_file and video are switched on nowhere by default
	// (TestDirectUploadPurposesOpenOnlyWhereSwitchedOnHTTP).
	requireCode(t, sendJSON(t, reviewedApp, organizer, fiber.MethodPost, "/v1/uploads", startBody("club_file", "a.zip", 100)),
		fiber.StatusUnprocessableEntity, "purpose_not_available")
	requireCode(t, sendJSON(t, reviewedApp, organizer, fiber.MethodPost, "/v1/uploads", startBody("video", "a.mp4", 100)),
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
		media.ServiceOptions{Catalogue: directCatalogue(t), ServiceProducts: open.ServiceClients.Products(),
			Direct: media.DirectUploadConfig{Purposes: openDirectPurposes}})
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

// A Direct upload purpose core attaches opens only where its side switches
// it on (MEDIA_DIRECT_UPLOAD_PURPOSES): none by default, whatever else is
// true. So ClamAV going live for Answer files, which are single-step and do
// not read the switch, opens no club file. A purpose switched off after an
// upload started is refused at its completion too.
func TestDirectUploadPurposesOpenOnlyWhereSwitchedOnHTTP(t *testing.T) {
	t.Parallel()
	keys := testauth.New(t)
	organizer := organizerToken(t, keys)
	// The reviewed catalogue and a malware scanner, as with ClamAV live;
	// no R2, so a start that passes every purpose rule goes no further.
	withSwitch := func(purposes ...string) *fiber.App {
		deps := memoryDeps()
		deps.ParseToken = keys.Parse()
		deps.Media = media.NewServiceWithOptions(media.NewMemoryStore(), media.NewMemoryBlob(), authz.NewAuthorizer(authz.DefaultPolicy()), "",
			media.ServiceOptions{ServiceProducts: deps.ServiceClients.Products(), Scans: idleScans{},
				Direct: media.DirectUploadConfig{Purposes: purposes}})
		return httpx.New(deps)
	}
	for _, tc := range []struct {
		purposes []string
		open     map[string]bool
	}{
		{nil, map[string]bool{"club_file": false, "video": false}},
		{[]string{"video"}, map[string]bool{"club_file": false, "video": true}},
		{[]string{"club_file", "video"}, map[string]bool{"club_file": true, "video": true}},
	} {
		app := withSwitch(tc.purposes...)
		for purpose, open := range tc.open {
			got := sendJSON(t, app, organizer, fiber.MethodPost, "/v1/uploads", startBody(purpose, "a.bin", 100))
			if open {
				requireCode(t, got, fiber.StatusServiceUnavailable, "direct_upload_unavailable")
			} else {
				requireCode(t, got, fiber.StatusUnprocessableEntity, "purpose_not_available")
			}
		}
	}

	// An Answer file, single-step, is uploaded with the scanner and nothing
	// switched on: it is scanned, not refused.
	bao := transittest.NewServer(t)
	store := media.NewMemoryStore()
	answers := memoryDeps()
	answers.ParseToken = keys.Parse()
	answers.Media = media.NewServiceWithOptions(store, media.NewMemoryBlob(), authz.NewAuthorizer(authz.DefaultPolicy()), "",
		media.ServiceOptions{
			ServiceProducts: answers.ServiceClients.Products(),
			Private: &media.PrivateMedia{
				Storage: media.NewPrivateStorage(media.NewMemoryBlob(), transit.New(bao.Config())),
				LinkKey: bytes.Repeat([]byte{5}, 32), LinkOrigin: "https://api.example.test",
				AccessLog: store, Now: bao.Clock.Now,
			},
			Scans: idleScans{},
		})
	answer := uploadAs(t, httpx.New(answers), newEditor(t, keys).token, "answer_file")
	if stored, err := store.Get(context.Background(), uuid.MustParse(answer)); err != nil || stored.Status != media.StatusScanning {
		t.Fatalf("the Answer file is %+v (err %v), want scanning", stored, err)
	}

	e := newDirectEnv(t, media.DefaultDirectUploadLimits())
	organizer = organizerToken(t, e.keys)
	file := pdfFile(1000)
	started := sendJSON(t, e.app, organizer, fiber.MethodPost, "/v1/uploads", startBody("club_file", "a.pdf", len(file)))
	if started.status != fiber.StatusCreated {
		t.Fatalf("start: status %d body %v", started.status, started.body)
	}
	body := completeBody(t, sendParts(t, partsOf(t, started.body), file))
	switchedOff := e.appWithPurposes(t, directCatalogue(t), "video")
	requireCode(t, sendJSON(t, switchedOff, organizer, fiber.MethodPost, "/v1/uploads/"+started.body["id"].(string)+"/complete", body),
		fiber.StatusUnprocessableEntity, "purpose_not_available")
}

// Every upload (single-step with a purpose, without one, as a profile
// picture, and by Direct upload) keeps the file name the browser sent,
// emoji, other scripts and decomposed letters included, and refuses only a
// name with a control character or a bidirectional formatting control, with
// media_name_invalid: a right-to-left override shows "a\u202Egnp.exe" as
// "aexe.png". A byte order mark is dropped.
func TestUploadFileNamesHTTP(t *testing.T) {
	t.Parallel()
	keys := testauth.New(t)
	deps := memoryDeps()
	deps.ParseToken = keys.Parse()
	deps.Media = media.NewServiceWithOptions(media.NewMemoryStore(), media.NewMemoryBlob(), authz.NewAuthorizer(authz.DefaultPolicy()), "",
		media.ServiceOptions{Catalogue: directCatalogue(t), ServiceProducts: deps.ServiceClients.Products(),
			Direct: media.DirectUploadConfig{Purposes: openDirectPurposes}})
	app := httpx.New(deps)
	organizer := organizerToken(t, keys)
	post := func(path, purpose, name string) jsonResponse {
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
		defer resp.Body.Close()
		got := map[string]any{}
		_ = json.NewDecoder(resp.Body).Decode(&got)
		return jsonResponse{status: resp.StatusCode, body: got}
	}
	singleSteps := []struct{ path, purpose string }{
		{"/v1/media", "event_cover"}, {"/v1/media", ""}, {"/v1/users/me/profile-picture", ""},
	}
	startNamed := func(name string) jsonResponse {
		t.Helper()
		raw, err := json.Marshal(map[string]any{"purpose": "club_file", "name": name, "size": 100})
		if err != nil {
			t.Fatal(err)
		}
		return sendJSON(t, app, organizer, fiber.MethodPost, "/v1/uploads", string(raw))
	}

	for label, name := range map[string]string{
		"Turkish":                "Kişisel Öğrenci Çalışması İĞÜŞÖÇı.png",
		"decomposed (NFD)":       "Gu\u0308ls\u0327en o\u0308dev.png",
		"emoji":                  "😀 ❤️ 👍🏽.png",
		"ZWJ emoji":              "👩\u200d💻 dev.png",
		"ZWJ family":             "👨\u200d👩\u200d👧.png",
		"rainbow flag":           "🏳️\u200d🌈.png",
		"England flag (tags)":    "🏴\U000E0067\U000E0062\U000E0065\U000E006E\U000E0067\U000E007F.png",
		"Persian ZWNJ":           "می\u200cخواهم.png",
		"soft hyphen":            "Ab\u00adschluss.png",
		"macOS screenshot NNBSP": "Screenshot 2026-09-28 at 11.48.00\u202fAM.png",
		"word joiner":            "a\u2060b.png",
	} {
		for _, upload := range singleSteps {
			got := post(upload.path, upload.purpose, name)
			if got.status != fiber.StatusCreated && got.status != fiber.StatusOK {
				t.Errorf("%s: %s (%q): status %d body %v", label, upload.path, upload.purpose, got.status, got.body)
			}
		}
		if got := post("/v1/media", "event_cover", name); got.body["name"] != name {
			t.Errorf("%s: kept as %q", label, got.body["name"])
		}
		// Direct upload has no R2 here: a name it accepts gets as far as that.
		requireCode(t, startNamed(name), fiber.StatusServiceUnavailable, "direct_upload_unavailable")
	}

	// A byte order mark is not part of the name.
	if got := post("/v1/media", "event_cover", "\ufeffrapor.png"); got.status != fiber.StatusCreated || got.body["name"] != "rapor.png" {
		t.Errorf("BOM: status %d name %q", got.status, got.body["name"])
	}

	refused := []string{
		"a\u202egnp.exe",  // right-to-left override
		"a\u202ab.png",    // left-to-right embedding
		"kapak\u2069.png", // pop directional isolate
		"kapak\u2068.png", // first strong isolate
		"a\u200eb.png",    // left-to-right mark
		"a\u200fb.png",    // right-to-left mark
		"a\u061cb.png",    // Arabic letter mark
		"a\tb.png",        // tab
		"a\u0085b.png",    // C1 next line
	}
	for _, name := range refused {
		for _, upload := range singleSteps {
			requireCode(t, post(upload.path, upload.purpose, name), fiber.StatusBadRequest, "media_name_invalid")
		}
	}
	// A multipart header cannot carry a line break or NUL; JSON can.
	for _, name := range append(refused, "a\nb.png", "a\rb.png", "a\x00b.png", "", "   ") {
		requireCode(t, startNamed(name), fiber.StatusBadRequest, "media_name_invalid")
	}
}
