package account_test

import (
	"testing"
	"time"

	"github.com/skylab-kulubu/core-backend/internal/account"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// awaitingManualStep tells Account Center that an operator carries out a
// service's step by hand (the service answered 404: no erase endpoint yet,
// today the CMS's), so the person's retry cannot help and is not offered.
// Any other manual intervention is the person's to retry, as before.
func TestSelfDeletionViewSaysWhenAnOperatorCarriesOutAStepByHand(t *testing.T) {
	t.Parallel()
	blocked := time.Date(2026, time.November, 1, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name    string
		status  user.DeletionRequestStatus
		blocked *time.Time
		code    string
		want    bool
	}{
		{"the CMS has no endpoint", user.DeletionRequestManualIntervention, &blocked, "erase_cms_rejected_404", true},
		{"Forms has no endpoint", user.DeletionRequestManualIntervention, &blocked, "erase_forms_rejected_404", true},
		{"SkyMail has no endpoint", user.DeletionRequestManualIntervention, &blocked, "erase_skymail_rejected_404", true},
		{"a refused role", user.DeletionRequestManualIntervention, &blocked, "erase_cms_rejected_403", false},
		{"a service down past the budget", user.DeletionRequestManualIntervention, &blocked, "erase_forms_failed", false},
		{"Keycloak down past the budget", user.DeletionRequestManualIntervention, &blocked, "delete_identity_failed", false},
		{"a core step's 404 is no service's", user.DeletionRequestManualIntervention, &blocked, "anonymize_core_rejected_404", false},
		{"back to pending after the operator", user.DeletionRequestPending, &blocked, "erase_cms_rejected_404", false},
		{"not blocked yet", user.DeletionRequestManualIntervention, nil, "erase_cms_rejected_404", false},
	} {
		view := account.SelfDeletionViewOf(user.SelfDeletionRecord{Request: user.DeletionRequest{
			Status: c.status, PlatformBlockedAt: c.blocked, LastErrorCode: c.code,
		}})
		if view.AwaitingManualStep != c.want {
			t.Errorf("%s: awaitingManualStep %v, want %v", c.name, view.AwaitingManualStep, c.want)
		}
	}
}
