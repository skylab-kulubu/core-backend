package httpx_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/httpx"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/testauth"
	"github.com/skylab-kulubu/core-backend/internal/transit"
	"github.com/skylab-kulubu/core-backend/internal/transit/transittest"
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
