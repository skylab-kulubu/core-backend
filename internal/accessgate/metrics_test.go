package accessgate_test

import (
	"strings"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/accessgate"
)

func TestMetricsRenderOnlyFixedAggregateCounters(t *testing.T) {
	t.Parallel()
	metrics := accessgate.NewMetrics()
	metrics.RecordDecision(accessgate.Allowed)
	metrics.RecordDecision(accessgate.Blocked)
	metrics.RecordDecision(accessgate.Unavailable)
	metrics.RecordRedisUnavailable()
	metrics.RecordContractMismatch()
	metrics.RecordMarkerMalformed()
	metrics.RecordReconciliationDrift()
	metrics.RecordPositiveTTL()
	metrics.RecordReadinessFailure()

	got := metrics.Prometheus()
	for _, line := range []string{
		"skylab_account_access_decisions_allowed_total 1",
		"skylab_account_access_decisions_blocked_total 1",
		"skylab_account_access_decisions_unavailable_total 1",
		"skylab_account_access_redis_unavailable_total 1",
		"skylab_account_access_contract_mismatch_total 1",
		"skylab_account_access_marker_malformed_total 1",
		"skylab_account_access_reconciliation_drift_total 1",
		"skylab_account_access_positive_ttl_total 1",
		"skylab_account_access_readiness_failure_total 1",
	} {
		if !strings.Contains(got, line+"\n") {
			t.Fatalf("metrics missing %q:\n%s", line, got)
		}
	}
	for _, forbidden := range []string{
		"11111111-1111-1111-1111-111111111111",
		accessgate.MarkerKeyPrefix,
		"digest",
		"subject",
		"{",
	} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("metrics exposed forbidden value %q:\n%s", forbidden, got)
		}
	}
}
