package httpx_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/clamd"
	"github.com/skylab-kulubu/core-backend/internal/clamd/clamdtest"
	"github.com/skylab-kulubu/core-backend/internal/httpx"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/testauth"
	"github.com/skylab-kulubu/core-backend/internal/transit"
	"github.com/skylab-kulubu/core-backend/internal/transit/transittest"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// idleScans is a malware scan that is configured but never runs: every
// scanned upload waits scanning.
type idleScans struct{}

func (idleScans) Wake() {}

// An Answer file waiting for its malware scan is not opened: Skyforms gets
// 409 media_scanning for a read link (and can show the file as pending),
// and 410 media_rejected, with the reason, once the scan rejected it.
func TestReadLinkWaitsForTheMalwareScanHTTP(t *testing.T) {
	t.Parallel()
	keys := testauth.New(t)
	bao := transittest.NewServer(t)
	store := media.NewMemoryStore()
	deps := memoryDeps()
	deps.ParseToken = keys.Parse()
	deps.Media = media.NewServiceWithOptions(store, media.NewMemoryBlob(), authz.NewAuthorizer(authz.DefaultPolicy()), "",
		media.ServiceOptions{
			ServiceProducts: deps.ServiceClients.Products(),
			Private: &media.PrivateMedia{
				Storage: media.NewPrivateStorage(media.NewMemoryBlob(), transit.New(bao.Config())),
				LinkKey: bytes.Repeat([]byte{5}, 32), LinkOrigin: "https://api.example.test",
				AccessLog: store, Now: bao.Clock.Now,
			},
			Scans: idleScans{},
		})
	app := httpx.New(deps)
	respondent := newEditor(t, keys)
	forms := serviceToken(t, keys, "forms", "media:attach")
	reviewer := uuid.NewString()

	scanning := uploadAs(t, app, respondent.token, "answer_file")
	link := sendJSON(t, app, forms, fiber.MethodPost, "/v1/media/"+scanning+"/links", `{"onBehalfOf":"`+reviewer+`"}`)
	if link.status != fiber.StatusConflict || link.body["code"] != "media_scanning" {
		t.Fatalf("link to a scanning Answer file: status %d body %v", link.status, link.body)
	}
	if got := store.ReadLinkLog(uuid.MustParse(scanning)); len(got) != 0 {
		t.Fatalf("a refused link was logged: %+v", got)
	}
	meta := sendJSON(t, app, forms, fiber.MethodGet, "/v1/media/"+scanning, "")
	if meta.status != fiber.StatusOK || meta.body["status"] != "scanning" || meta.body["url"] != "" {
		t.Fatalf("Skyforms reads the scanning Answer file as %d %v", meta.status, meta.body)
	}

	purged := time.Now().UTC()
	rejected, err := store.Create(context.Background(), media.Media{
		Name: "cv.pdf", Type: "application/pdf", Key: "private/files/" + uuid.NewString(), Size: 10,
		UploadedBy: uuid.New(), Kind: media.KindFile, Purpose: media.PurposeAnswerFile,
		Status: media.StatusRejected, ScanResult: media.ScanInfected, Visibility: media.VisibilityPrivate,
		Encryption:         &media.Encryption{Algorithm: "aes-256-gcm-chunked-v1", WrappedKey: "vault:v1:AAAA", KeyVersion: 1},
		BlobPurgeStartedAt: &purged, BlobPurgedAt: &purged,
	})
	if err != nil {
		t.Fatal(err)
	}
	gone := sendJSON(t, app, forms, fiber.MethodPost, "/v1/media/"+rejected.ID.String()+"/links", `{"onBehalfOf":"`+reviewer+`"}`)
	if gone.status != fiber.StatusGone || gone.body["code"] != "media_rejected" || gone.body["scanResult"] != "infected" {
		t.Fatalf("link to a rejected Answer file: status %d body %v", gone.status, gone.body)
	}
	meta = sendJSON(t, app, forms, fiber.MethodGet, "/v1/media/"+rejected.ID.String(), "")
	if meta.status != fiber.StatusOK || meta.body["status"] != "rejected" || meta.body["scanResult"] != "infected" {
		t.Fatalf("Skyforms reads the rejected Answer file as %d %v", meta.status, meta.body)
	}
}

// scannedDirectApp is e's core with club_file needing its malware scan (as
// the reviewed catalogue has it), and a scan worker over the same database
// and storage with a fake clamd.
func scannedDirectApp(t *testing.T, e *directEnv) (*fiber.App, *media.ScanWorker, *clamdtest.Server) {
	t.Helper()
	fake := clamdtest.New(t)
	worker, err := media.NewScanWorker(media.ScanWorkerConfig{Store: e.store, Scanner: clamd.New(fake.Addr()), Public: e.r2})
	if err != nil {
		t.Fatal(err)
	}
	deps := memoryDeps()
	deps.Users = user.NewService(user.NewPostgresStore(e.pool))
	deps.ParseToken = e.keys.Parse()
	deps.MediaUploadLimiter = e.singleStep
	deps.Media = media.NewServiceWithOptions(e.store, e.r2, authz.NewAuthorizer(authz.DefaultPolicy()), "https://cdn.example.test",
		media.ServiceOptions{
			Catalogue:       catalogueWith(t, "club_file", func(entry map[string]any) { entry["scan"] = true }),
			ServiceProducts: deps.ServiceClients.Products(),
			Direct:          media.DirectUploadConfig{Storage: e.r2, Limiter: e.limiter, Now: e.clock.Now, Purposes: openDirectPurposes},
			Scans:           worker,
		})
	return httpx.New(deps), worker, fake
}

// A public file that needs a malware scan is never served before it is
// clean. The completion holds it under pending/, at a key only core knows
// (the pending key the browser wrote to is gone, and nothing is at a files/
// key), as an opaque download without a name; the Media has no address.
// Once clean, the file is copied to its served key with the serving
// policy's metadata and the Media gets its address. An infected one is
// deleted and never reaches a files/ key.
func TestDirectUploadOfAScannedPublicFileIsServedOnlyOnceCleanHTTP(t *testing.T) {
	e := newDirectEnv(t, media.DefaultDirectUploadLimits())
	app, worker, _ := scannedDirectApp(t, e)
	e.app = app
	organizer := organizerToken(t, e.keys)

	file := pdfFile(partSize + 4321)
	done := e.upload(t, organizer, "club_file", "etkinlik notları.pdf", file)
	if done.status != fiber.StatusCreated || done.body["status"] != "scanning" || done.body["url"] != "" || done.body["sizes"] != nil {
		t.Fatalf("complete: status %d body %v", done.status, done.body)
	}
	id := done.body["id"].(string)
	keys := e.s3.Keys("media")
	if len(keys) != 1 || !strings.HasPrefix(keys[0], "pending/scan/") || strings.Contains(keys[0], id) {
		t.Fatalf("stored while scanning: %v, want one held key under pending/scan/ that does not name the upload", keys)
	}
	held, _ := e.s3.Object("media", keys[0])
	if held.ContentType != "application/octet-stream" || held.ContentDisposition != "attachment" || !bytes.Equal(held.Data, file) {
		t.Fatalf("held as %q %q (%d bytes)", held.ContentType, held.ContentDisposition, len(held.Data))
	}
	for _, seen := range []jsonResponse{anonymousGet(t, e.app, "/v1/media/"+id), sendJSON(t, e.app, organizer, fiber.MethodGet, "/v1/media/"+id, "")} {
		if seen.status != fiber.StatusOK || seen.body["url"] != "" {
			t.Fatalf("read while scanning: status %d body %v", seen.status, seen.body)
		}
	}

	report, err := worker.Pass(context.Background(), func(err error) { t.Error(err) })
	if err != nil || report.Clean != 1 {
		t.Fatalf("scan %+v, err %v", report, err)
	}
	served, ok := e.s3.Object("media", "files/"+id)
	if !ok || served.ContentType != "application/pdf" || !bytes.Equal(served.Data, file) {
		t.Fatalf("served copy %v: %q, %d bytes", ok, served.ContentType, len(served.Data))
	}
	if keys := e.s3.Keys("media"); len(keys) != 1 || keys[0] != "files/"+id {
		t.Fatalf("stored once clean: %v", keys)
	}
	seen := anonymousGet(t, e.app, "/v1/media/"+id)
	if seen.status != fiber.StatusOK || seen.body["url"] != "https://cdn.example.test/files/"+id {
		t.Fatalf("read once clean: status %d body %v", seen.status, seen.body)
	}
	if mine := sendJSON(t, e.app, organizer, fiber.MethodGet, "/v1/media/"+id, ""); mine.body["status"] != "pending" || mine.body["scanResult"] != "clean" {
		t.Fatalf("uploader reads %v", mine.body)
	}

	infected := append(pdfFile(1000), clamd.EICAR()...)
	bad := e.upload(t, organizer, "club_file", "virüs.pdf", infected)
	if bad.status != fiber.StatusCreated || bad.body["status"] != "scanning" {
		t.Fatalf("complete: status %d body %v", bad.status, bad.body)
	}
	if report, err := worker.Pass(context.Background(), func(err error) { t.Error(err) }); err != nil || report.Rejected != 1 {
		t.Fatalf("scan %+v, err %v", report, err)
	}
	if keys := e.s3.Keys("media"); len(keys) != 1 || keys[0] != "files/"+id {
		t.Fatalf("stored after the rejection: %v", keys)
	}
	told := sendJSON(t, e.app, organizer, fiber.MethodGet, "/v1/media/"+bad.body["id"].(string), "")
	if told.body["status"] != "rejected" || told.body["scanResult"] != "infected" || told.body["url"] != "" {
		t.Fatalf("the uploader reads %v", told.body)
	}
}

// anonymousGet reads path without a token.
func anonymousGet(t *testing.T, app *fiber.App, path string) jsonResponse {
	t.Helper()
	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, path, nil), fiber.TestConfig{Timeout: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got := map[string]any{}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("status %d: %v", resp.StatusCode, err)
	}
	return jsonResponse{status: resp.StatusCode, body: got}
}
