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
	"github.com/skylab-kulubu/core-backend/internal/zipcheck"
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
	// scanLeaseMargin is how long a scan's lease outlasts its work: the
	// work stops that long before the lease ends.
	scanLeaseMargin = 2 * time.Minute
	// ScanDeadline is how long a Media may wait scanning after its upload.
	// Past it, the Media is rejected as scan_timeout: its scan never ended
	// (clamd down for a week, or a file that keeps failing), and the
	// uploader must upload it again.
	ScanDeadline = 7 * 24 * time.Hour
)

// scanTimeout bounds one file's scan: reading it from storage (decrypting
// a private one) while clamd reads it, and clamd's verdict. Two minutes,
// and a second more for every MiB (1 GiB: about 19 minutes).
func scanTimeout(size int64) time.Duration {
	return 2*time.Minute + time.Duration(size>>20)*time.Second
}

// zipCheckWork bounds the ZIP check of a file of size bytes
// (checkContent), which reads every file once more before clamd: the
// longest the check may take under the limits (the file read twice, what it
// inflates to never more than MaxScanSize; the check itself stops sooner,
// after the time what it inflates to calls for: zipcheck.Timeout), and the
// storage calls before it.
func (w *ScanWorker) zipCheckWork(size int64) time.Duration {
	return w.limits.MaxTimeout(size) + 2*scanStorageTimeout
}

// scanWork bounds everything a claim does: the ZIP check, the scan, the
// copy of a clean held file, and the storage calls around them.
func (w *ScanWorker) scanWork(m Media) time.Duration {
	return w.zipCheckWork(m.Size) + scanTimeout(m.Size) + scanCopyTimeout + 4*scanStorageTimeout
}

// lease is how long a scan's claim holds its Media.
func (w *ScanWorker) lease(m Media) time.Duration {
	return w.scanWork(m) + scanLeaseMargin
}

// scanHoldPrefix starts the key a public file waiting for its scan is held
// at, in the public bucket: under the pending prefix, which the R2
// lifecycle rule clears after two days and the CDN refuses, at a random key
// only core knows. Once clean, it is copied to its served key
// (servedKeyOf).
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

// servedKeyOf is where a held file of the Media is served from once clean.
// It is the Media's id, so a copy retried after a crash lands on the same
// key, and so every purge of a held Media knows it (purgeMediaObjects); only
// clean bytes are ever written there. No video is ever held: a video needs
// no scan (ErrCeilingVideoScan), and its served key keeps its extension
// (directServedKey).
func servedKeyOf(id uuid.UUID) string {
	return "files/" + id.String()
}

// purgeMediaObjects deletes every object the Media at key may have: its
// object and its sizes (purgeObjects); for a file held until its scan ends,
// the clean copy a scan may have made at its served key already (a scan
// that copied it and then found its Media gone, or crashed before recording
// it); and for a video, its original, whichever key the Media points at:
// a purge's delete of it (purgeDeleter) takes every faststart copy beside
// it too. An object that is not there is deleted already. The staging
// sweeper deletes by purgeObjects alone, with a plain delete: it deletes
// only a key no Media points at, and a video's copies are written only for
// an original its Media points at, whose own purge takes them.
func purgeMediaObjects(id uuid.UUID, key string, purge func(key string) error) error {
	if err := purgeObjects(key, purge); err != nil {
		return err
	}
	if isScanHoldKey(key) {
		return purge(servedKeyOf(id))
	}
	if original, ok := videoOriginalOf(key); ok && original != key {
		return purge(original)
	}
	return nil
}

// ErrScannerDown is a pass that stopped because clamd cannot be reached:
// every Media waits scanning, and is tried again.
var ErrScannerDown = errors.New("media: the malware scanner cannot be reached")

// ErrPrivateZIPUnchecked is the scan of a private ZIP larger than
// ScanLimits.MaxBuffer put off: the ZIP check decrypts a private file into
// memory, and reads a larger one by ranged GETs only once private Direct
// upload offers them (ticket 21). The Media is never scanned unchecked: it
// waits scanning, is tried again, and is rejected as scan_timeout at its
// scan deadline.
var ErrPrivateZIPUnchecked = errors.New("media: a private ZIP larger than the ZIP check keeps in memory cannot be checked yet, so it is not scanned")

// ScanRejection reports a Media the scan rejected for a reason clamd did
// not name: its private object failing its integrity check, or a ZIP the ZIP
// check refused, Err saying why (members by their place, never a file
// name).
type ScanRejection struct {
	ID     uuid.UUID
	Result ScanResult
	Err    error
}

func (r *ScanRejection) Error() string {
	return fmt.Sprintf("media %s rejected as %s: %v", r.ID, r.Result, r.Err)
}

func (r *ScanRejection) Unwrap() error { return r.Err }

// scanRejections are the errors that end a scan in a rejection, in the
// order they are looked for, the result each gives, and whether the pass
// reports it (ScanRejection).
var scanRejections = []struct {
	err    error
	result ScanResult
	report bool
}{
	// The held file is gone (the R2 lifecycle rule clears pending/ after
	// two days) or the object is: it can never be scanned.
	{ErrNotFound, ScanLost, false},
	{ErrPrivateIntegrity, ScanIntegrity, true},
	{clamd.ErrStreamTooLarge, ScanTooLarge, false},
	{zipcheck.ErrTooLarge, ScanTooLarge, true},
	{zipcheck.ErrInvalid, ScanArchiveInvalid, true},
	{zipcheck.ErrNested, ScanArchiveNested, true},
}

// Scanner is clamd (clamd.Client): it reads a whole file and names what it
// found in it, if anything.
type Scanner interface {
	Scan(ctx context.Context, r io.Reader) (clamd.Result, error)
}

// ScanStorage is the public bucket as the scan uses it (R2): it streams a
// held file, reads a ZIP's ranges for the ZIP check, copies a clean one to
// its served key, rewrites that copy's metadata, and deletes.
type ScanStorage interface {
	Open(ctx context.Context, key string) (io.ReadCloser, error)
	// Size is the stored object's size; ErrNotFound when there is none.
	Size(ctx context.Context, key string) (int64, error)
	// OpenRange streams the n bytes of the object from off (a ranged GET);
	// ErrNotFound when there is none.
	OpenRange(ctx context.Context, key string, off, n int64) (io.ReadCloser, error)
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
	// Limits are clamd's archive limits, which a ZIP is held within before
	// it is scanned (zipcheck); DefaultScanLimits when left out. Limits set
	// only in part are refused (NewScanWorker).
	Limits ScanLimits
}

// ScanWorker is the malware scan (media redesign ticket 12): it streams the
// plaintext of each Media waiting for its scan to clamd, a private one
// decrypted as it streams (nothing is written to disk), and moves it on:
//
//   - clean: pending, or attached when a Media attachment already links it;
//     a public file held apart is copied to its served key first;
//   - infected, too large for clamd to scan, its file lost (the held object
//     is gone), or its private object failing its integrity check:
//     rejected, its objects deleted from the bucket that holds them, and the
//     rejection recorded (Media, reason, signature, time; no file name);
//   - a ZIP clamd could not scan whole (the ZIP check, zipcheck, on any
//     file whose content is a ZIP, before any byte of it reaches clamd):
//     rejected the same way, as too_large_to_scan, as archive_invalid when
//     it is malformed or holds what clamd cannot read, or as
//     archive_nested when it holds an archive core cannot check;
//   - still scanning ScanDeadline after its upload: rejected as
//     scan_timeout, whether clamd answers or not;
//   - clamd unreachable: the pass stops and every Media waits scanning;
//   - any other failure (clamd answered an error, storage failed): the
//     Media waits scanning and is tried again later, each failure in a row
//     doubling the wait (DeferScan).
//
// Each Media is claimed before any clamd or storage work (ClaimNextScan), so
// the core replicas a rolling deploy runs side by side never scan the same
// Media, and a claim whose work ran past its lease moves nothing on. A pass
// walks the due Media by id, and a Media that fails never holds up the
// others. Every step is idempotent: a pass cut short is made again.
type ScanWorker struct {
	store   *PostgresStore
	scanner Scanner
	public  ScanStorage
	private *PrivateStorage
	limits  ScanLimits
	now     func() time.Time
	wake    chan struct{}
}

// NewScanWorker makes the scan worker. Its limits are DefaultScanLimits
// when left out; limits set only in part are refused, so that core does not
// start with them.
func NewScanWorker(config ScanWorkerConfig) (*ScanWorker, error) {
	now := config.Now
	if now == nil {
		now = time.Now
	}
	limits := config.Limits
	if limits == (ScanLimits{}) {
		limits = DefaultScanLimits
	}
	if err := limits.Validate(); err != nil {
		return nil, fmt.Errorf("media scan limits: %w", err)
	}
	return &ScanWorker{
		store: config.Store, scanner: config.Scanner, public: config.Public, private: config.Private,
		limits: limits, now: now, wake: make(chan struct{}, 1),
	}, nil
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
	Clean int
	// Rejected are the Media the pass rejected, scan_timeout included.
	Rejected int
	// Finished are rejected Media whose objects an earlier pass could not
	// delete, deleted in this one.
	Finished int
	Failed   int
}

func (r ScanReport) any() bool {
	return r.Clean+r.Rejected+r.Finished+r.Failed > 0
}

// Pass makes one pass over the Media due for the scan: it first rejects the
// ones past ScanDeadline, then claims and moves on each due Media in id
// order. A Media that fails is reported through onError and tried again
// later; one the pass rejects for a reason clamd did not give (its private
// object failing its integrity check, a ZIP the ZIP check refuses, with
// why) is reported too. It stops with ErrScannerDown when clamd cannot be
// reached.
func (w *ScanWorker) Pass(ctx context.Context, onError func(error)) (ScanReport, error) {
	var report ScanReport
	now := w.now().UTC()
	overdue, err := w.store.RejectOverdueScans(ctx, now, ScanDeadline)
	if err != nil {
		return report, err
	}
	report.Rejected += overdue
	after := uuid.Nil
	for {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		claim, found, err := w.store.ClaimNextScan(ctx, w.now().UTC(), after, w.lease)
		if err != nil || !found {
			return report, err
		}
		after = claim.Media.ID
		err = w.step(ctx, claim, &report, onError)
		if errors.Is(err, clamd.ErrUnreachable) {
			return report, errors.Join(fmt.Errorf("%w: %w", ErrScannerDown, err), w.release(ctx, claim))
		}
		if err != nil {
			report.Failed++
			if onError != nil {
				onError(&BackfillError{Kind: "media", ID: claim.Media.ID, Err: err})
			}
			w.deferScan(ctx, claim, onError)
		}
	}
}

func (w *ScanWorker) release(ctx context.Context, claim ScanClaim) error {
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), scanStorageTimeout)
	defer cancel()
	return w.store.ReleaseScan(releaseCtx, claim)
}

func (w *ScanWorker) deferScan(ctx context.Context, claim ScanClaim, onError func(error)) {
	deferCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), scanStorageTimeout)
	defer cancel()
	if err := w.store.DeferScan(deferCtx, claim, w.now().UTC()); err != nil && onError != nil {
		onError(&BackfillError{Kind: "media", ID: claim.Media.ID, Err: err})
	}
}

// step moves one claimed Media on, within its claim's work time.
func (w *ScanWorker) step(ctx context.Context, claim ScanClaim, report *ScanReport, onError func(error)) error {
	ctx, cancel := context.WithTimeout(ctx, w.scanWork(claim.Media))
	defer cancel()
	m := claim.Media
	if m.Status == StatusRejected {
		done, err := w.finishRejection(ctx, m)
		if done {
			report.Finished++
		}
		return err
	}
	verdict, err := w.scan(ctx, m)
	if err != nil {
		for _, r := range scanRejections {
			if errors.Is(err, r.err) {
				if r.report && onError != nil {
					onError(&ScanRejection{ID: m.ID, Result: r.result, Err: err})
				}
				return w.reject(ctx, claim, r.result, "", report)
			}
		}
		return err
	}
	if verdict.Infected() {
		result := ScanInfected
		if strings.HasPrefix(verdict.Signature, "Heuristics.Limits.Exceeded") {
			// clamd could not scan all of it (AlertExceedsMax).
			result = ScanTooLarge
		}
		return w.reject(ctx, claim, result, verdict.Signature, report)
	}
	return w.clean(ctx, claim, report)
}

// scan streams the Media's plaintext to clamd, once the ZIP check has
// passed it.
func (w *ScanWorker) scan(ctx context.Context, m Media) (clamd.Result, error) {
	if err := w.checkContent(ctx, m); err != nil {
		return clamd.Result{}, err
	}
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

// checkContent refuses a file clamd could not scan whole (zipcheck, media
// redesign ticket 23): clamd reads an archive member that inflates past its
// MaxFileSize only up to MaxFileSize, without a report, in a ZIP and in an
// archive it finds inside another file (an Office document, a ZIP appended
// to a PDF or an image).
// So every file is read once before clamd, whatever its type says: a file
// whose content is a ZIP is checked whole, any other is searched for
// archives past its first byte. It runs under the scan's claim, holding no
// database connection. A public held file is read by ranged GETs. A private
// file is decrypted as it streams, into nothing but memory: a ZIP is kept
// there to be checked, at most MaxBuffer (ErrPrivateZIPUnchecked beyond).
func (w *ScanWorker) checkContent(ctx context.Context, m Media) error {
	ctx, cancel := context.WithTimeout(ctx, w.zipCheckWork(m.Size))
	defer cancel()
	if sealed, ok := m.Sealed(); ok {
		if w.private == nil {
			return ErrPrivateMediaDisabled
		}
		body, err := w.private.Open(ctx, sealed)
		if err != nil {
			return fmt.Errorf("open the file: %w", err)
		}
		defer body.Close()
		err = zipcheck.CheckStream(ctx, body, m.Size, w.limits)
		if errors.Is(err, zipcheck.ErrZIPTooLargeToKeep) {
			return ErrPrivateZIPUnchecked
		}
		return err
	}
	if w.public == nil {
		return ErrDirectUploadUnavailable
	}
	size, err := w.public.Size(ctx, m.Key)
	if err != nil {
		return fmt.Errorf("size the file: %w", err)
	}
	return zipcheck.CheckFile(ctx, heldRanges{storage: w.public, key: m.Key}, size, w.limits)
}

// heldRanges is a held file as the ZIP check reads it.
type heldRanges struct {
	storage ScanStorage
	key     string
}

func (h heldRanges) OpenRange(ctx context.Context, off, n int64) (io.ReadCloser, error) {
	return h.storage.OpenRange(ctx, h.key, off, n)
}

// clean ends the scan of a clean Media: a held public file is copied to
// its served key before the Media points there, and the held copy then
// goes.
func (w *ScanWorker) clean(ctx context.Context, claim ScanClaim, report *ScanReport) error {
	m := claim.Media
	if !isScanHoldKey(m.Key) {
		done, err := w.store.MarkScanClean(ctx, claim, m.Key, w.now().UTC())
		if done {
			report.Clean++
		}
		return err
	}
	served, err := w.copyToServed(ctx, m)
	if err != nil {
		return err
	}
	done, err := w.store.MarkScanClean(ctx, claim, served, w.now().UTC())
	if err != nil {
		return err
	}
	if !done {
		return w.dropUnservedCopy(ctx, m.ID, served)
	}
	report.Clean++
	// The R2 lifecycle rule clears pending/ should this fail.
	_ = w.deletePublic(ctx, m.Key)
	return w.keepErasedName(ctx, m, served)
}

// dropUnservedCopy is a clean copy's end when its Media was not marked
// clean. The claim may be lost to another worker, which serves the same key,
// or the Media archived: the copy stays (a restored Media is scanned and
// copied again; its archive purge deletes the copy with it). But a Media
// whose purge has begun can never be marked clean again, and a purge that
// did not wait for this claim (account erasure, or one past the lease) may
// have deleted the served key before the copy landed: the copy goes. An
// object already gone counts as deleted.
func (w *ScanWorker) dropUnservedCopy(ctx context.Context, id uuid.UUID, served string) error {
	now, err := w.store.GetIncludingDeleted(ctx, id)
	if err != nil || (now.BlobPurgeStartedAt == nil && now.BlobPurgedAt == nil) {
		return err
	}
	return w.deletePublic(ctx, served)
}

// copyToServed copies a clean held file to its served key with the serving
// policy's metadata.
func (w *ScanWorker) copyToServed(ctx context.Context, m Media) (string, error) {
	if w.public == nil {
		return "", ErrDirectUploadUnavailable
	}
	served := servedKeyOf(m.ID)
	copyCtx, cancel := context.WithTimeout(ctx, scanCopyTimeout)
	defer cancel()
	if err := w.public.Copy(copyCtx, m.Key, served, ServingMetadataFor(m.Purpose, m.Type, m.Name)); err != nil {
		return "", fmt.Errorf("copy the clean file to its served key: %w", err)
	}
	return served, nil
}

// keepErasedName rewrites the served copy's metadata when an account
// erasure cleared the Media's name after the copy read it (as the serving
// policy backfill does, it reads the Media again).
func (w *ScanWorker) keepErasedName(ctx context.Context, m Media, served string) error {
	now, err := w.store.GetIncludingDeleted(ctx, m.ID)
	if err != nil || now.Name == m.Name {
		return err
	}
	metaCtx, cancel := context.WithTimeout(ctx, scanStorageTimeout)
	defer cancel()
	if err := w.public.SetMetadata(metaCtx, served, ServingMetadataFor(now.Purpose, now.Type, now.Name)); err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	return nil
}

// reject rejects the Media under its claim and deletes its objects.
func (w *ScanWorker) reject(ctx context.Context, claim ScanClaim, result ScanResult, signature string, report *ScanReport) error {
	done, err := w.store.RejectScanned(ctx, claim, result, signature, w.now().UTC())
	if err != nil || !done {
		return err
	}
	report.Rejected++
	_, err = w.finishRejection(ctx, claim.Media)
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
// nothing. The returned channel closes once the pass in flight has
// returned after ctx ends, its claims settled (they are written even then);
// shutdown waits on it before it closes the pool.
func (w *ScanWorker) Run(ctx context.Context, logf func(format string, args ...any)) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
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
			if downWait > 0 && err == nil {
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
	return done
}
