package media_test

import (
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/media"
)

// Direct upload's own budget (decision Q23): 3 uploads open at once and
// 10 GiB declared per rolling day, per person, by default.
func TestDirectUploadLimitsFromEnv(t *testing.T) {
	t.Parallel()
	limits, err := media.DirectUploadLimitsFromEnv(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if limits != (media.DirectUploadLimits{MaxOpen: 3, DailyBytes: 10 << 30}) || limits != media.DefaultDirectUploadLimits() {
		t.Fatalf("defaults %+v", limits)
	}
	values := map[string]string{"MEDIA_DIRECT_UPLOAD_MAX_OPEN": "5", "MEDIA_DIRECT_UPLOAD_DAILY_MAX_MIB": "4096"}
	limits, err = media.DirectUploadLimitsFromEnv(func(key string) string { return values[key] })
	if err != nil || limits != (media.DirectUploadLimits{MaxOpen: 5, DailyBytes: 4 << 30}) {
		t.Fatalf("limits %+v err %v", limits, err)
	}
	for key, bad := range map[string]string{
		"MEDIA_DIRECT_UPLOAD_MAX_OPEN":      "0",
		"MEDIA_DIRECT_UPLOAD_DAILY_MAX_MIB": "lots",
	} {
		values := map[string]string{key: bad}
		if _, err := media.DirectUploadLimitsFromEnv(func(key string) string { return values[key] }); err == nil {
			t.Fatalf("%s=%s must be rejected", key, bad)
		}
	}
}
