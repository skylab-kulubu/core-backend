package media_test

import (
	"testing"
	"time"

	"github.com/skylab-kulubu/core-backend/internal/media"
)

func TestUploadLimitsDefaultToTheSpec(t *testing.T) {
	t.Parallel()
	limits, err := media.UploadLimitsFromEnv(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	want := media.UploadLimits{Count: 30, CountWindow: 10 * time.Minute, DailyBytes: 500 << 20}
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
