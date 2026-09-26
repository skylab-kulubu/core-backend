package media

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	DefaultBlobRecoveryWindow = 30 * 24 * time.Hour
	defaultBlobPurgeInterval  = time.Hour
	defaultBlobPurgeBatchSize = 25
	blobPurgeCandidateTimeout = 30 * time.Second
)

type BlobPurgeConfig struct {
	RecoveryWindow time.Duration
	Interval       time.Duration
	BatchSize      int
}

func BlobPurgeConfigFromEnv(getenv func(string) string) (BlobPurgeConfig, error) {
	config := BlobPurgeConfig{
		RecoveryWindow: DefaultBlobRecoveryWindow,
		Interval:       defaultBlobPurgeInterval,
		BatchSize:      defaultBlobPurgeBatchSize,
	}
	if raw := strings.TrimSpace(getenv("MEDIA_BLOB_RECOVERY_DAYS")); raw != "" {
		days, err := strconv.Atoi(raw)
		if err != nil || days <= 0 {
			return BlobPurgeConfig{}, fmt.Errorf("MEDIA_BLOB_RECOVERY_DAYS must be a positive integer")
		}
		config.RecoveryWindow = time.Duration(days) * 24 * time.Hour
	}
	if raw := strings.TrimSpace(getenv("MEDIA_BLOB_PURGE_INTERVAL")); raw != "" {
		interval, err := time.ParseDuration(raw)
		if err != nil || interval <= 0 {
			return BlobPurgeConfig{}, fmt.Errorf("MEDIA_BLOB_PURGE_INTERVAL must be a positive duration")
		}
		config.Interval = interval
	}
	if raw := strings.TrimSpace(getenv("MEDIA_BLOB_PURGE_BATCH_SIZE")); raw != "" {
		batch, err := strconv.Atoi(raw)
		if err != nil || batch <= 0 {
			return BlobPurgeConfig{}, fmt.Errorf("MEDIA_BLOB_PURGE_BATCH_SIZE must be a positive integer")
		}
		config.BatchSize = batch
	}
	return config, nil
}

type PurgeReport struct {
	Scanned    int
	Purged     int
	Referenced int
}

// PurgeDeleted removes blobs only after the recovery window and after the
// store has checked all durable media references under a database lock.
func PurgeDeleted(ctx context.Context, store Store, blobs BlobStore, now time.Time, recoveryWindow time.Duration, limit int) (PurgeReport, error) {
	items, err := store.ListPurgeCandidates(ctx, now.Add(-recoveryWindow), limit)
	if err != nil {
		return PurgeReport{}, err
	}
	report := PurgeReport{Scanned: len(items)}
	for _, item := range items {
		candidateCtx, cancel := context.WithTimeout(ctx, blobPurgeCandidateTimeout)
		purged, err := store.PurgeBlobIfUnreferenced(candidateCtx, item.ID, now, func(key string) error {
			return blobs.Delete(candidateCtx, key)
		})
		cancel()
		if err != nil {
			return report, err
		}
		if purged {
			report.Purged++
		} else {
			report.Referenced++
		}
	}
	return report, nil
}

// ExpiryReport counts one pass of the expiry cleanup.
type ExpiryReport struct {
	Purged int
	// Kept are expired Media left in place: attached again since they were
	// listed, or still used by a record without a Media attachment.
	Kept   int
	Failed int
}

// PurgeExpired makes one pass over the Media no Media attachment keeps whose
// expiry is at or before now: pending Media past their purpose's pending TTL
// and detached Media past their 30 days. Each blob goes through the same
// locked reference check and two-phase claim as PurgeDeleted, and the Media
// is archived as its blob goes. Attached Media, legacy Media (no expiry) and
// archived Media are never purged here. The pass walks the Media by id in
// batches (BackfillPass): a Media whose purge fails is reported through
// onError and retried on the next pass, and never holds up the others.
func PurgeExpired(ctx context.Context, store Store, blobs BlobStore, now time.Time, onError func(error)) (ExpiryReport, error) {
	var report ExpiryReport
	pass, err := BackfillPass(ctx, "media",
		func(ctx context.Context, after uuid.UUID, limit int) ([]Media, error) {
			return store.ListExpired(ctx, now, after, limit)
		},
		func(item Media) uuid.UUID { return item.ID },
		func(ctx context.Context, item Media) error {
			candidateCtx, cancel := context.WithTimeout(ctx, blobPurgeCandidateTimeout)
			defer cancel()
			purged, err := store.PurgeExpiredBlobIfUnattached(candidateCtx, item.ID, now, func(key string) error {
				return blobs.Delete(candidateCtx, key)
			})
			if err != nil {
				return err
			}
			if purged {
				report.Purged++
			} else {
				report.Kept++
			}
			return nil
		},
		onError)
	report.Failed = pass.Failed
	return report, err
}

// MaintainBlobPurge runs one bounded batch of archived Media, then a pass of
// the expiry cleanup, immediately and then at the configured interval. The
// returned channel closes after ctx is cancelled.
func MaintainBlobPurge(ctx context.Context, store Store, blobs BlobStore, config BlobPurgeConfig, onError func(error)) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		timer := time.NewTimer(0)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				if _, err := PurgeDeleted(ctx, store, blobs, time.Now().UTC(), config.RecoveryWindow, config.BatchSize); err != nil && onError != nil {
					onError(err)
				}
				if _, err := PurgeExpired(ctx, store, blobs, time.Now().UTC(), onError); err != nil && onError != nil {
					onError(err)
				}
				timer.Reset(config.Interval)
			}
		}
	}()
	return done
}
