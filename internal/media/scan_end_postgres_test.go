package media_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/clamd/clamdtest"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

// Every scan ends. A held file that is gone (the R2 lifecycle rule clears
// pending/ after two days, while clamd was down) can never be scanned: its
// Media is rejected as lost, not retried forever, attached or not.
func TestPostgresAScanWhoseFileIsGoneEndsLost(t *testing.T) {
	d := newScanDatabase(t)
	club := d.heldClubFile(t, "data.pdf", pdfFile())
	if _, err := d.pool.Exec(context.Background(), `UPDATE media SET expires_at = NULL WHERE id = $1`, club.ID); err != nil {
		t.Fatal(err)
	}
	if err := d.blobs.Delete(context.Background(), club.Key); err != nil {
		t.Fatal(err)
	}
	if report := d.pass(t); report.Rejected != 1 || report.Failed != 0 {
		t.Fatalf("report %+v", report)
	}
	got := d.get(t, club.ID)
	if got.Status != media.StatusRejected || got.ScanResult != media.ScanLost || got.BlobPurgedAt == nil {
		t.Fatalf("%s (%s), purged %v", got.Status, got.ScanResult, got.BlobPurgedAt)
	}
	if r := rejectionOf(t, d, club.ID); r.result != media.ScanLost || r.signature != "" {
		t.Fatalf("rejection %+v", r)
	}
}

// A private object that fails its integrity check while it is read for the
// scan is rejected as integrity, logged by the Media's id alone, and not
// retried.
func TestPostgresAPrivateObjectThatFailsItsCheckIsRejected(t *testing.T) {
	d := newScanDatabase(t)
	created := d.answer(t, pdfFile())
	stored, _ := d.private.Get(created.Key)
	broken := append([]byte{}, stored...)
	broken[len(broken)-1] ^= 0xff
	if err := d.private.Put(context.Background(), created.Key, broken, media.BlobMetadata{ContentType: "application/octet-stream"}); err != nil {
		t.Fatal(err)
	}
	var logged []string
	report, err := d.worker.Pass(context.Background(), func(err error) { logged = append(logged, err.Error()) })
	if err != nil || report.Rejected != 1 || report.Failed != 0 {
		t.Fatalf("report %+v, err %v", report, err)
	}
	got := d.get(t, created.ID)
	if got.Status != media.StatusRejected || got.ScanResult != media.ScanIntegrity || got.BlobPurgedAt == nil {
		t.Fatalf("%s (%s), purged %v", got.Status, got.ScanResult, got.BlobPurgedAt)
	}
	if _, ok := d.private.Get(created.Key); ok {
		t.Fatal("the broken object is still stored")
	}
	if len(logged) != 1 || !strings.Contains(logged[0], created.ID.String()) || !strings.Contains(logged[0], "integrity") || strings.Contains(logged[0], created.Key) {
		t.Fatalf("logged %q", logged)
	}
}

// A Media still waiting for its scan a week after its upload is rejected
// as scan_timeout, even while clamd cannot be reached: attached or not, it
// does not wait forever. One whose scan is under way (a live claim) is left
// to it.
func TestPostgresAScanPastItsDeadlineIsRejected(t *testing.T) {
	d := newScanDatabase(t)
	ctx := context.Background()
	stale := d.answer(t, pdfFile())
	d.attachFor(t, formsService, stale, media.Owner{Service: authz.ProductForms, Type: "response", ID: uuid.NewString()}, media.RoleFormsAnswer)
	fresh := d.answer(t, pdfFile())
	if _, err := d.pool.Exec(ctx, `UPDATE media SET created_at = $2 WHERE id = $1`, stale.ID, d.now.Add(-media.ScanDeadline-time.Minute)); err != nil {
		t.Fatal(err)
	}
	d.clamd.Stop()

	report, err := d.worker.Pass(ctx, func(err error) { t.Logf("scan: %v", err) })
	if err == nil || report.Rejected != 1 {
		t.Fatalf("report %+v, err %v; want the overdue one rejected and the pass stopped at clamd", report, err)
	}
	got := d.get(t, stale.ID)
	if got.Status != media.StatusRejected || got.ScanResult != media.ScanTimeout {
		t.Fatalf("overdue: %s (%s)", got.Status, got.ScanResult)
	}
	if r := rejectionOf(t, d, stale.ID); r.result != media.ScanTimeout {
		t.Fatalf("rejection %+v", r)
	}
	if got := d.get(t, fresh.ID); got.Status != media.StatusScanning {
		t.Fatalf("within its deadline: %s", got.Status)
	}
	// Once clamd answers, a pass deletes the overdue one's object and scans
	// the other.
	d.client.Addr = clamdtest.New(t).Addr()
	d.pass(t)
	if _, ok := d.private.Get(stale.Key); ok {
		t.Fatal("the overdue object is still stored")
	}
	if got := d.get(t, fresh.ID); got.Status != media.StatusPending {
		t.Fatalf("within its deadline, once scanned: %s", got.Status)
	}
}
