package accessgate

import (
	"strconv"
	"strings"
	"sync/atomic"
)

// Metrics contains only fixed aggregate counters. It deliberately has no
// label-bearing API, so account identifiers and Redis keys cannot become
// metric dimensions.
type Metrics struct {
	allowed             atomic.Uint64
	blocked             atomic.Uint64
	unavailable         atomic.Uint64
	redisUnavailable    atomic.Uint64
	contractMismatch    atomic.Uint64
	markerMalformed     atomic.Uint64
	reconciliationDrift atomic.Uint64
	positiveTTL         atomic.Uint64
	readinessFailure    atomic.Uint64
}

func NewMetrics() *Metrics { return &Metrics{} }

func (m *Metrics) RecordDecision(decision Decision) {
	if m == nil {
		return
	}
	switch decision {
	case Allowed:
		m.allowed.Add(1)
	case Blocked:
		m.blocked.Add(1)
	default:
		m.unavailable.Add(1)
	}
}

func (m *Metrics) RecordRedisUnavailable() {
	if m != nil {
		m.redisUnavailable.Add(1)
	}
}

func (m *Metrics) RecordContractMismatch() {
	if m != nil {
		m.contractMismatch.Add(1)
	}
}

func (m *Metrics) RecordMarkerMalformed() {
	if m != nil {
		m.markerMalformed.Add(1)
	}
}

func (m *Metrics) RecordReconciliationDrift() {
	if m != nil {
		m.reconciliationDrift.Add(1)
	}
}

func (m *Metrics) RecordPositiveTTL() {
	if m != nil {
		m.positiveTTL.Add(1)
	}
}

func (m *Metrics) RecordReadinessFailure() {
	if m != nil {
		m.readinessFailure.Add(1)
	}
}

func (m *Metrics) Prometheus() string {
	if m == nil {
		return ""
	}
	values := []struct {
		name  string
		value uint64
	}{
		{"skylab_account_access_decisions_allowed_total", m.allowed.Load()},
		{"skylab_account_access_decisions_blocked_total", m.blocked.Load()},
		{"skylab_account_access_decisions_unavailable_total", m.unavailable.Load()},
		{"skylab_account_access_redis_unavailable_total", m.redisUnavailable.Load()},
		{"skylab_account_access_contract_mismatch_total", m.contractMismatch.Load()},
		{"skylab_account_access_marker_malformed_total", m.markerMalformed.Load()},
		{"skylab_account_access_reconciliation_drift_total", m.reconciliationDrift.Load()},
		{"skylab_account_access_positive_ttl_total", m.positiveTTL.Load()},
		{"skylab_account_access_readiness_failure_total", m.readinessFailure.Load()},
	}
	var out strings.Builder
	for _, metric := range values {
		out.WriteString(metric.name)
		out.WriteByte(' ')
		out.WriteString(strconv.FormatUint(metric.value, 10))
		out.WriteByte('\n')
	}
	return out.String()
}
