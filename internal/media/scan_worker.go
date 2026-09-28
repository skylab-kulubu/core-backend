package media

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/clamd"
)

const (
	// scanPollInterval is how often the scan worker looks for Media
	// waiting for their scan when no upload nudges it.
	scanPollInterval = 30 * time.Second
	// scanRetryFirst and scanRetryMax bound the wait before a Media whose
	// scan failed is tried again; each failure in a row doubles it.
	scanRetryFirst = 30 * time.Second
	scanRetryMax   = time.Hour
	// scannerDownFirst and scannerDownMax bound the wait before a pass
	// after one that found clamd unreachable.
	scannerDownFirst = 10 * time.Second
	scannerDownMax   = 5 * time.Minute
	// scanStorageTimeout bounds a storage call that moves no file.
	scanStorageTimeout = 30 * time.Second
	// scanCopyTimeout bounds copying a clean held file (at most
	// MaxScanBytes) to its served key, inside R2.
	scanCopyTimeout = 12 * time.Minute
)

// scanTimeout bounds one file's scan: reading it from storage (decrypting
// a private one) while clamd reads it, and clamd's verdict. Two minutes,
// and a second more for every MiB (1 GiB: about 19 minutes).
func scanTimeout(size int64) time.Duration {
	return 2*time.Minute + time.Duration(size>>20)*time.Second
}

// scanHoldPrefix starts the key a public file waiting for its scan is held
// at, in the public bucket: under the pending prefix, which the R2
// lifecycle rule clears after two days and the CDN may refuse, at a random
// key only core knows. Once clean, it is copied to its served key
// (servedKey).
const scanHoldPrefix = pendingKeyPrefix + "scan/"

// scanHoldKey is a new key to hold a public file at until its scan ends.
func scanHoldKey() string {
	return scanHoldPrefix + uuid.NewString()
}

func isScanHoldKey(key string) bool {
	return strings.HasPrefix(key, scanHoldPrefix)
}

// scanHoldMetadata is what a held file is stored with: an opaque download
// without a name.
var scanHoldMetadata = BlobMetadata{ContentType: "application/octet-stream", ContentDisposition: "attachment"}

// servedKey is where a held file is served from once clean. It is the
// Media's id, so a copy retried after a crash lands on the same key; only
// clean bytes are ever written there.
func servedKey(m Media) string {
	return "files/" + m.ID.String()
}

// ErrScannerDown is a pass that stopped because clamd cannot be reached:
// every Media waits scanning, and is tried again.
var ErrScannerDown = errors.New("media: the malware scanner cannot be reached")

// Scanner is clamd (clamd.Client): it reads a whole file and names what it
// found in it, if anything.
type Scanner interface {
	Scan(ctx context.Context, r io.Reader) (clamd.Result, error)
}

// ScanStorage is the public bucket as the scan uses it (R2): it streams a
// held file, copies a clean one to its served key, rewrites that copy's
// metadata, and deletes.
type ScanStorage interface {
	Open(ctx context.Context, key string) (io.ReadCloser, error)
	Copy(ctx context.Context, from, to string, meta BlobMetadata) error
	SetMetadata(ctx context.Context, key string, meta BlobMetadata) error
	Delete(ctx context.Context, key string) error
}

// ScanWorkerConfig is what the scan worker needs.
type ScanWorkerConfig struct {
	Store   *PostgresStore
	Scanner Scanner
	// Public is the public bucket; nil in a core without R2, where no
	// public file waits for a scan (only Direct upload holds one).
	Public ScanStorage
	// Private opens and deletes private Media; nil while private Media is
	// off.
	Private *PrivateStorage
	// Now defaults to time.Now.
	Now func() time.Time
}

// ScanWorker is the malware scan (media redesign ticket 12): it streams the
// plaintext of each Media waiting for its scan to clamd, a private one
// decrypted as it streams (nothing is written to disk), and moves it on:
//
//   - clean: pending, or attached when a Media attachment already links it;
//     a public file held apart is copied to its served key first;
//   - infected, or too large for clamd to scan: rejected, its objects
//     deleted from the bucket that holds them, and the rejection recorded
//     (Media, reason, signature, time; no file name);
//   - clamd unreachable: the pass stops and every Media waits scanning;
//   - any other failure (clamd answered an error, storage failed): the
//     Media waits scanning and is tried again later, each failure in a row
//     doubling the wait (DeferScan).
//
// A pass walks the due Media by id in batches, and a Media that fails never
// holds up the others. Every step is idempotent: a pass cut short is made
// again.
type ScanWorker struct {
	store   *PostgresStore
	scanner Scanner
	public  ScanStorage
	private *PrivateStorage
	now     func() time.Time
	wake    chan struct{}
}

func NewScanWorker(config ScanWorkerConfig) *ScanWorker {
	now := config.Now
	if now == nil {
		now = time.Now
	}
	return &ScanWorker{
		store: config.Store, scanner: config.Scanner, public: config.Public, private: config.Private,
		now: now, wake: make(chan struct{}, 1),
	}
}

// Wake asks the worker for a pass now (ScanQueue).
func (w *ScanWorker) Wake() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// ScanReport counts one pass.
type ScanReport struct {
	Clean    int
	Rejected int
	// Finished are rejected Media whose objects an earlier pass could not
	// delete, deleted in this one.
	Finished int
	Failed   int
}

func (r ScanReport) any() bool {
	return r.Clean+r.Rejected+r.Finished+r.Failed > 0
}

// Pass makes one pass over the Media due for the scan. A Media that fails
// is reported through onError and tried again later. It stops with
// ErrScannerDown when clamd cannot be reached.
func (w *ScanWorker) Pass(ctx context.Context, onError func(error)) (ScanReport, error) {
	var report ScanReport
	passCtx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	listedAt := w.now().UTC()
	_, err := BackfillPass(passCtx, "media",
		func(ctx context.Context, after uuid.UUID, limit int) ([]Media, error) {
			return w.store.ListScanDue(ctx, listedAt, after, limit)
		},
		func(m Media) uuid.UUID { return m.ID },
		func(ctx context.Context, m Media) error {
			err := w.step(ctx, m, &report)
			if errors.Is(err, clamd.ErrUnreachable) {
				stop(fmt.Errorf("%w: %w", ErrScannerDown, err))
				return nil
			}
			if err != nil {
				report.Failed++
				deferCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), scanStorageTimeout)
				err = errors.Join(err, w.store.DeferScan(deferCtx, m.ID, w.now().UTC()))
				cancel()
			}
			return err
		},
		onError)
	if cause := context.Cause(passCtx); errors.Is(cause, ErrScannerDown) {
		return report, cause
	}
	return report, err
}

// step moves one due Media on.
func (w *ScanWorker) step(ctx context.Context, m Media, report *ScanReport) error {
	if m.Status == StatusRejected {
		done, err := w.finishRejection(ctx, m)
		if done {
			report.Finished++
		}
		return err
	}
	verdict, err := w.scan(ctx, m)
	switch {
	case errors.Is(err, clamd.ErrStreamTooLarge):
		return w.reject(ctx, m, ScanTooLarge, "", report)
	case err != nil:
		return err
	case verdict.Infected():
		result := ScanInfected
		if strings.HasPrefix(verdict.Signature, "Heuristics.Limits.Exceeded") {
			// clamd could not scan all of it (AlertExceedsMax).
			result = ScanTooLarge
		}
		return w.reject(ctx, m, result, verdict.Signature, report)
	}
	return w.clean(ctx, m, report)
}

// scan streams the Media's plaintext to clamd.
func (w *ScanWorker) scan(ctx context.Context, m Media) (clamd.Result, error) {
	scanCtx, cancel := context.WithTimeout(ctx, scanTimeout(m.Size))
	defer cancel()
	var body io.ReadCloser
	var err error
	if sealed, ok := m.Sealed(); ok {
		if w.private == nil {
			return clamd.Result{}, ErrPrivateMediaDisabled
		}
		body, err = w.private.Open(scanCtx, sealed)
	} else {
		if w.public == nil {
			return clamd.Result{}, ErrDirectUploadUnavailable
		}
		body, err = w.public.Open(scanCtx, m.Key)
	}
	if err != nil {
		return clamd.Result{}, fmt.Errorf("open the file: %w", err)
	}
	defer body.Close()
	return w.scanner.Scan(scanCtx, body)
}

// clean ends the scan of a clean Media. A held public file is copied to its
// served key, with the serving policy's metadata, before the Media points
// there; the held copy then goes.
func (w *ScanWorker) clean(ctx context.Context, m Media, report *ScanReport) error {
	served := m.Key
	if isScanHoldKey(m.Key) {
		if w.public == nil {
			return ErrDirectUploadUnavailable
		}
		served = servedKey(m)
		copyCtx, cancel := context.WithTimeout(ctx, scanCopyTimeout)
		err := w.public.Copy(copyCtx, m.Key, served, ServingMetadata(m.Type, m.Name))
		cancel()
		if err != nil {
			return fmt.Errorf("copy the clean file to its served key: %w", err)
		}
	}
	done, err := w.store.MarkScanClean(ctx, m.ID, m.Key, served, w.now().UTC())
	if err != nil {
		// A held file's copy stays: the next pass copies onto it again.
		return err
	}
	if served == m.Key {
		if done {
			report.Clean++
		}
		return nil
	}
	if !done {
		// The Media is gone or archived meanwhile: nothing serves the copy.
		return w.deletePublic(ctx, served)
	}
	report.Clean++
	// The R2 lifecycle rule clears pending/ should this fail.
	_ = w.deletePublic(ctx, m.Key)
	// The copy's download name was read before it; an account erasure may
	// have cleared it since (as the serving policy backfill does, read the
	// Media again).
	now, err := w.store.GetIncludingDeleted(ctx, m.ID)
	if err != nil || now.Name == m.Name {
		return err
	}
	metaCtx, cancel := context.WithTimeout(ctx, scanStorageTimeout)
	defer cancel()
	if err := w.public.SetMetadata(metaCtx, served, ServingMetadata(now.Type, now.Name)); err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	return nil
}

// reject rejects the Media and deletes its objects.
func (w *ScanWorker) reject(ctx context.Context, m Media, result ScanResult, signature string, report *ScanReport) error {
	done, err := w.store.RejectScanned(ctx, m.ID, m.Key, result, signature, w.now().UTC())
	if err != nil || !done {
		return err
	}
	report.Rejected++
	_, err = w.finishRejection(ctx, m)
	return err
}

// finishRejection deletes a rejected Media's objects from the bucket that
// holds them.
func (w *ScanWorker) finishRejection(ctx context.Context, m Media) (bool, error) {
	return w.store.FinishScanRejection(ctx, m.ID, w.now().UTC(), func(key string) error {
		if isPrivateKey(key) {
			if w.private == nil {
				return ErrPrivateMediaDisabled
			}
			deleteCtx, cancel := context.WithTimeout(ctx, scanStorageTimeout)
			defer cancel()
			return w.private.Delete(deleteCtx, key)
		}
		return w.deletePublic(ctx, key)
	})
}

func (w *ScanWorker) deletePublic(ctx context.Context, key string) error {
	if w.public == nil {
		return ErrDirectUploadUnavailable
	}
	deleteCtx, cancel := context.WithTimeout(ctx, scanStorageTimeout)
	defer cancel()
	return w.public.Delete(deleteCtx, key)
}

// Run makes passes in the background until ctx ends: at once, whenever an
// upload wakes it, and every scanPollInterval. While clamd cannot be
// reached it waits longer between passes (up to scannerDownMax). It logs
// what a pass changed and each Media that failed, and says once that clamd
// is down and once that it is back; a pass with nothing to do says
// nothing.
func (w *ScanWorker) Run(ctx context.Context, logf func(format string, args ...any)) {
	go func() {
		timer := time.NewTimer(0)
		defer timer.Stop()
		var downWait time.Duration
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			case <-w.wake:
			}
			report, err := w.Pass(ctx, func(err error) { logf("media scan: %v", err) })
			next := scanPollInterval
			switch {
			case errors.Is(err, ErrScannerDown):
				if downWait == 0 {
					logf("media scan: %v; Media wait scanning until it answers", err)
				}
				downWait = min(max(2*downWait, scannerDownFirst), scannerDownMax)
				next = downWait
			case err != nil && ctx.Err() == nil:
				logf("media scan: %v", err)
			}
			if downWait > 0 && !errors.Is(err, ErrScannerDown) && err == nil {
				logf("media scan: clamd answers again")
				downWait = 0
			}
			if report.any() {
				logf("media scan: %d clean, %d rejected, %d rejections finished, %d failed (tried again later)",
					report.Clean, report.Rejected, report.Finished, report.Failed)
			}
			timer.Reset(next)
		}
	}()
}
