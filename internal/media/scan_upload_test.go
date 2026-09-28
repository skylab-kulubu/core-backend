package media_test

import (
	"bytes"
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/transit"
	"github.com/skylab-kulubu/core-backend/internal/transit/transittest"
)

// wakes counts the nudges uploads give the scan worker.
type wakes struct{ n atomic.Int32 }

func (w *wakes) Wake() { w.n.Add(1) }

// newScannedMedia is privateMedia with a malware scanner configured and the
// reviewed catalogue: an Answer file needs its scan.
func newScannedMedia(t *testing.T) (privateMedia, *wakes) {
	t.Helper()
	bao := transittest.NewServer(t)
	store := media.NewMemoryStore()
	public, private := media.NewMemoryBlob(), media.NewMemoryBlob()
	storage := media.NewPrivateStorage(private, transit.New(bao.Config()))
	queue := &wakes{}
	svc := media.NewServiceWithOptions(store, public, authz.NewAuthorizer(authz.DefaultPolicy()), "https://cdn.example.test",
		media.ServiceOptions{
			ServiceProducts: []authz.Product{authz.ProductForms},
			Private: &media.PrivateMedia{
				Storage: storage, LinkKey: bytes.Repeat([]byte{7}, 32), LinkOrigin: "https://api.example.test/",
				AccessLog: store, Now: bao.Clock.Now,
			},
			Scans: queue,
		})
	return privateMedia{svc: svc, store: store, public: public, private: private, storage: storage, bao: bao}, queue
}

// With a scanner, the scan gate is lifted: an Answer file is stored,
// encrypted, and waits for its scan (scanning) with its purpose's pending
// expiry. The scan worker is nudged.
func TestService_WithAScannerAnAnswerFileWaitsForItsScan(t *testing.T) {
	t.Parallel()
	pm, queue := newScannedMedia(t)
	ctx := context.Background()
	before := time.Now().UTC()

	created, err := pm.svc.UploadForPurpose(ctx, signedIn("61616161-6161-6161-6161-616161616161"), "answer_file", uploaded("cv.pdf", "application/pdf", pdfFile()))
	if err != nil {
		t.Fatal(err)
	}
	if created.Status != media.StatusScanning || created.ScanResult != "" {
		t.Fatalf("created %s with scan result %q, want scanning", created.Status, created.ScanResult)
	}
	if created.ExpiresAt == nil || created.ExpiresAt.Before(before.Add(24*time.Hour)) || created.ExpiresAt.After(time.Now().Add(24*time.Hour)) {
		t.Fatalf("expires %v, want the purpose's 24 hours", created.ExpiresAt)
	}
	if _, ok := pm.private.Get(created.Key); !ok {
		t.Fatal("the answer file was not stored")
	}
	if got := queue.n.Load(); got != 1 {
		t.Fatalf("the scan worker was nudged %d times", got)
	}

	// A purpose that needs no scan is stored as before, without a nudge.
	picture, err := pm.svc.UploadForPurpose(ctx, signedIn("61616161-6161-6161-6161-616161616161"), "profile_picture", uploaded("me.png", "image/png", twoTonePNG(t)))
	if err != nil {
		t.Fatal(err)
	}
	if picture.Status != media.StatusPending || queue.n.Load() != 1 {
		t.Fatalf("profile picture %s, nudges %d", picture.Status, queue.n.Load())
	}
}
