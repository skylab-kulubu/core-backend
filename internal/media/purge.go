package media

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
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

// MaintainBlobPurge runs one bounded batch immediately and then at the
// configured interval. The returned channel closes after ctx is cancelled.
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
				timer.Reset(config.Interval)
			}
		}
	}()
	return done
}
