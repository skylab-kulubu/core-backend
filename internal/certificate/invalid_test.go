package certificate

import (
	"errors"
	"fmt"
	"testing"
)

// A queued job's error code names an unavailable renderer, which the job
// tries again, instead of the catch-all processing_failed.
func TestSafeErrorCodeNamesAnUnavailableRenderer(t *testing.T) {
	t.Parallel()
	for err, want := range map[error]string{
		fmt.Errorf("render: %w", rendererUnavailable(errors.New("dial tcp: connection refused"))): CodeRendererUnavailable,
		invalid(CodeBackgroundPDFPages): "invalid_input",
		errors.New("boom"):              "processing_failed",
	} {
		if got := safeErrorCode(err); got != want {
			t.Errorf("safeErrorCode(%v) = %q, want %q", err, got, want)
		}
	}
}
