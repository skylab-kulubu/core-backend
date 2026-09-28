package media_test

import (
	"strings"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/media"
)

// A video (the video purpose's MP4) is served to play: video/mp4, inline, so
// a <video> element and the browser's player take it (the CDN answers Range
// requests, and a Cloudflare rule adds nosniff). An MP4 of any other purpose,
// or written without one, stays an opaque download under its name.
func TestServingPolicyPlaysOnlyTheVideoPurposesMP4(t *testing.T) {
	t.Parallel()
	if got := media.ServingMetadataFor(media.PurposeVideo, "video/mp4", "açılış.mp4"); got != (media.BlobMetadata{ContentType: "video/mp4"}) {
		t.Fatalf("a video is served as %+v, want video/mp4 inline", got)
	}
	download := media.ServingMetadata("video/mp4", "açılış.mp4")
	if download.ContentType != "application/octet-stream" || !strings.HasPrefix(download.ContentDisposition, "attachment; filename*=") {
		t.Fatalf("an MP4 without a purpose is served as %+v, want a download under its name", download)
	}
	for _, purpose := range []string{media.PurposeLegacy, media.PurposeClubFile, ""} {
		if got := media.ServingMetadataFor(purpose, "video/mp4", "açılış.mp4"); got != download {
			t.Errorf("an MP4 of %q is served as %+v, want %+v", purpose, got, download)
		}
	}
	// Every other type is served as the purpose-free policy serves it.
	for _, tc := range []struct{ contentType, name string }{
		{"application/pdf", "a.pdf"}, {"application/zip", "a.zip"}, {"image/svg+xml", "a.svg"}, {"image/png", "a.png"},
	} {
		if got, want := media.ServingMetadataFor(media.PurposeVideo, tc.contentType, tc.name), media.ServingMetadata(tc.contentType, tc.name); got != want {
			t.Errorf("%s for video: %+v, want %+v", tc.contentType, got, want)
		}
	}
}
