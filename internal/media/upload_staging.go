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
	DefaultUploadStagingGrace   = 24 * time.Hour
	defaultUploadSweepInterval  = 15 * time.Minute
	defaultUploadSweepBatchSize = 25
	minimumUploadStagingGrace   = 2 * time.Minute
	uploadStagingDeleteTimeout  = 30 * time.Second
)

type UploadStagingConfig struct {
	Grace     time.Duration
	Interval  time.Duration
	BatchSize int
}

func UploadStagingConfigFromEnv(getenv func(string) string) (UploadStagingConfig, error) {
	config := UploadStagingConfig{
		Grace: DefaultUploadStagingGrace, Interval: defaultUploadSweepInterval, BatchSize: defaultUploadSweepBatchSize,
	}
	if raw := strings.TrimSpace(getenv("MEDIA_UPLOAD_STAGING_GRACE")); raw != "" {
		grace, err := time.ParseDuration(raw)
		if err != nil || grace < minimumUploadStagingGrace {
			return UploadStagingConfig{}, fmt.Errorf("MEDIA_UPLOAD_STAGING_GRACE must be a duration of at least %s", minimumUploadStagingGrace)
		}
		config.Grace = grace
	}
	if raw := strings.TrimSpace(getenv("MEDIA_UPLOAD_STAGING_SWEEP_INTERVAL")); raw != "" {
		interval, err := time.ParseDuration(raw)
		if err != nil || interval <= 0 {
			return UploadStagingConfig{}, fmt.Errorf("MEDIA_UPLOAD_STAGING_SWEEP_INTERVAL must be a positive duration")
		}
		config.Interval = interval
	}
	if raw := strings.TrimSpace(getenv("MEDIA_UPLOAD_STAGING_BATCH_SIZE")); raw != "" {
		batch, err := strconv.Atoi(raw)
		if err != nil || batch <= 0 {
			return UploadStagingConfig{}, fmt.Errorf("MEDIA_UPLOAD_STAGING_BATCH_SIZE must be a positive integer")
		}
		config.BatchSize = batch
	}
	return config, nil
}

// UploadStagingStore makes the object write discoverable before it happens.
// CreateStaged must insert media metadata and remove the staging row in one
// transaction while holding the staging-row lock.
type UploadStagingStore interface {
	StageUpload(ctx context.Context, key string, subjectID uuid.UUID, cleanupAfter time.Time) error
	CreateStaged(ctx context.Context, item Media) (Media, error)
	ReadyStagedUploadForCleanup(ctx context.Context, key string, cleanupAfter time.Time) error
	CancelStagedUpload(ctx context.Context, key string) error
	PurgeNextStagedUpload(ctx context.Context, now time.Time, purge func(key string) error) (bool, error)
}

type UploadStagingPurgeReport struct {
	Scanned  int
	Resolved int
}

func PurgeStagedUploads(ctx context.Context, store UploadStagingStore, blobs BlobStore, now time.Time, limit int) (UploadStagingPurgeReport, error) {
	if limit <= 0 {
		return UploadStagingPurgeReport{}, ErrInvalid
	}
	report := UploadStagingPurgeReport{}
	for report.Scanned < limit {
		candidateCtx, cancel := context.WithTimeout(ctx, uploadStagingDeleteTimeout)
		found, err := store.PurgeNextStagedUpload(candidateCtx, now, func(key string) error {
			return blobs.Delete(candidateCtx, key)
		})
		cancel()
		if !found {
			return report, err
		}
		report.Scanned++
		if err != nil {
			return report, err
		}
		report.Resolved++
	}
	return report, nil
}

func MaintainUploadStaging(ctx context.Context, store UploadStagingStore, blobs BlobStore, config UploadStagingConfig, onError func(error)) <-chan struct{} {
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
				if _, err := PurgeStagedUploads(ctx, store, blobs, time.Now().UTC(), config.BatchSize); err != nil && onError != nil {
					onError(err)
				}
				timer.Reset(config.Interval)
			}
		}
	}()
	return done
}
