package media_test

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

// The defaults leave room for an organizer's gallery day; see "Upload limits"
// in docs/media-lifecycle.md.
func TestUploadLimitsDefaultTo100Per10MinutesAnd2GiBPerDay(t *testing.T) {
	t.Parallel()
	limits, err := media.UploadLimitsFromEnv(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	want := media.UploadLimits{Count: 100, CountWindow: 10 * time.Minute, DailyBytes: 2048 << 20}
	if limits != want {
		t.Fatalf("limits %+v; want %+v", limits, want)
	}
}

func TestUploadLimitsFromEnv(t *testing.T) {
	t.Parallel()
	values := map[string]string{
		"MEDIA_UPLOAD_RATE_MAX":      "12",
		"MEDIA_UPLOAD_RATE_WINDOW":   "1h",
		"MEDIA_UPLOAD_DAILY_MAX_MIB": "2048",
	}
	limits, err := media.UploadLimitsFromEnv(func(key string) string { return values[key] })
	if err != nil {
		t.Fatal(err)
	}
	want := media.UploadLimits{Count: 12, CountWindow: time.Hour, DailyBytes: 2 << 30}
	if limits != want {
		t.Fatalf("limits %+v; want %+v", limits, want)
	}

	for key, bad := range map[string]string{
		"MEDIA_UPLOAD_RATE_MAX":      "0",
		"MEDIA_UPLOAD_RATE_WINDOW":   "10",
		"MEDIA_UPLOAD_DAILY_MAX_MIB": "-1",
	} {
		values := map[string]string{key: bad}
		if _, err := media.UploadLimitsFromEnv(func(key string) string { return values[key] }); err == nil {
			t.Fatalf("%s=%s must be rejected", key, bad)
		}
	}
}

func TestServiceUploadLimitsDefaultTo1000Per10MinutesAnd10GiBPerDay(t *testing.T) {
	t.Parallel()
	limits, err := media.ServiceUploadLimitsFromEnv(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	want := media.UploadLimits{Count: 1000, CountWindow: 10 * time.Minute, DailyBytes: 10240 << 20}
	if limits != want {
		t.Fatalf("limits %+v; want %+v", limits, want)
	}
}

func TestServiceUploadLimitsFromEnv(t *testing.T) {
	t.Parallel()
	values := map[string]string{
		"MEDIA_SERVICE_UPLOAD_RATE_MAX":      "300",
		"MEDIA_SERVICE_UPLOAD_RATE_WINDOW":   "1m",
		"MEDIA_SERVICE_UPLOAD_DAILY_MAX_MIB": "4096",
		"MEDIA_UPLOAD_RATE_MAX":              "7",
	}
	limits, err := media.ServiceUploadLimitsFromEnv(func(key string) string { return values[key] })
	if err != nil {
		t.Fatal(err)
	}
	want := media.UploadLimits{Count: 300, CountWindow: time.Minute, DailyBytes: 4 << 30}
	if limits != want {
		t.Fatalf("limits %+v; want %+v", limits, want)
	}

	for key, bad := range map[string]string{
		"MEDIA_SERVICE_UPLOAD_RATE_MAX":      "0",
		"MEDIA_SERVICE_UPLOAD_RATE_WINDOW":   "10",
		"MEDIA_SERVICE_UPLOAD_DAILY_MAX_MIB": "-1",
	} {
		values := map[string]string{key: bad}
		_, err := media.ServiceUploadLimitsFromEnv(func(key string) string { return values[key] })
		if err == nil || !strings.HasPrefix(err.Error(), key+" ") {
			t.Fatalf("%s=%s must be rejected naming the variable, got %v", key, bad, err)
		}
	}
}

// The limiter holds a person only while they have an upload inside a window.
func TestUploadLimiterLetsGoOfPeopleWithNothingLeftToCount(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 26, 9, 0, 0, 0, time.UTC)
	limiter := media.NewUploadLimiter(media.UploadLimits{Count: 5, CountWindow: 10 * time.Minute, DailyBytes: 1000},
		func() time.Time { return now })
	ada, grace, linus := uuid.New(), uuid.New(), uuid.New()

	charge, refusal := limiter.Admit(ada, 100)
	if refusal != nil || limiter.Tracked() != 1 {
		t.Fatalf("admit: refusal %v, tracked %d", refusal, limiter.Tracked())
	}
	charge.Refund()
	if limiter.Tracked() != 0 {
		t.Fatalf("after its only upload was refunded, tracked %d", limiter.Tracked())
	}

	if _, refusal := limiter.Admit(grace, 5000); refusal == nil || limiter.Tracked() != 0 {
		t.Fatalf("an upload over the whole budget: refusal %v, tracked %d", refusal, limiter.Tracked())
	}

	if _, refusal := limiter.Admit(ada, 100); refusal != nil {
		t.Fatal(refusal)
	}
	now = now.Add(24 * time.Hour)
	if _, refusal := limiter.Admit(linus, 100); refusal != nil || limiter.Tracked() != 1 {
		t.Fatalf("a day later: refusal %v, tracked %d; want only the new uploader", refusal, limiter.Tracked())
	}
}
