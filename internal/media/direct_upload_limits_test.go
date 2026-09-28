package media_test

import (
	"slices"
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

// Which Direct upload purposes a side opens is its own switch
// (MEDIA_DIRECT_UPLOAD_PURPOSES), since the catalogue is the same file on
// sandbox and production: none by default, and only Direct upload purposes
// of the catalogue. A misspelt or single-step one stops core at startup.
func TestDirectUploadPurposesFromEnv(t *testing.T) {
	t.Parallel()
	catalogue, err := media.LoadCatalogue()
	if err != nil {
		t.Fatal(err)
	}
	for raw, want := range map[string][]string{
		"":                        nil,
		"  ":                      nil,
		"video":                   {"video"},
		" club_file , video ":     {"club_file", "video"},
		"video,,video,":           {"video"},
		"answer_file_large,video": {"answer_file_large", "video"},
	} {
		got, err := media.DirectUploadPurposesFromEnv(func(key string) string {
			if key == "MEDIA_DIRECT_UPLOAD_PURPOSES" {
				return raw
			}
			return ""
		}, catalogue)
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("%q: %v (err %v), want %v", raw, got, err, want)
		}
	}
	for _, bad := range []string{"videos", "event_cover", "answer_file", "legacy", "video;club_file"} {
		if _, err := media.DirectUploadPurposesFromEnv(func(string) string { return bad }, catalogue); err == nil {
			t.Errorf("MEDIA_DIRECT_UPLOAD_PURPOSES=%s must be rejected", bad)
		}
	}
}
