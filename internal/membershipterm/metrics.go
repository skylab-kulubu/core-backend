package membershipterm

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Refresh reads the gauges from the store: every replica reports the same,
// and a restart loses nothing. A read that fails keeps the last ones.
func (s *Service) Refresh(ctx context.Context) {
	readCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	counts, err := s.config.Store.Counts(readCtx, s.now())
	if err != nil {
		if ctx.Err() == nil {
			s.logf("management terms: gauges: %s", errorCode(err))
		}
		return
	}
	s.count(func() { s.counts, s.counted = counts, true })
}

// Prometheus is /v1/metrics' part of the terms: counts only, no person,
// Group or path.
func (s *Service) Prometheus() string {
	var out strings.Builder
	out.WriteString("# TYPE skylab_management_terms_mode gauge\n")
	mode := ModeOff
	if s != nil {
		mode = s.config.Mode
	}
	for _, m := range []Mode{ModeOff, ModeDryRun, ModeApply} {
		value := 0
		if m == mode {
			value = 1
		}
		fmt.Fprintf(&out, "skylab_management_terms_mode{mode=%q} %d\n", m, value)
	}
	if s == nil || mode == ModeOff {
		return out.String()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	gauge := func(name string, value int64) {
		fmt.Fprintf(&out, "# TYPE %s gauge\n%s %d\n", name, name, value)
	}
	counter := func(name string, value int64) {
		fmt.Fprintf(&out, "# TYPE %s counter\n%s %d\n", name, name, value)
	}
	flag := func(b bool) int64 {
		if b {
			return 1
		}
		return 0
	}
	c := s.counts
	gauge("skylab_management_terms_open", int64(c.Open))
	gauge("skylab_management_terms_undated", int64(c.Undated))
	gauge("skylab_management_terms_expiring", int64(c.Expiring))
	// The ticket's metric: expired terms whose holder is still in the Group.
	gauge("skylab_management_terms_expired_in_group", int64(c.ExpiredInGroup))
	var last int64
	if c.LastSuccess != nil {
		last = c.LastSuccess.Unix()
	}
	gauge("skylab_management_terms_last_success_timestamp_seconds", last)
	gauge("skylab_management_terms_last_run_failed", flag(c.LastRunFailed))
	gauge("skylab_management_terms_active_season", flag(c.SeasonFound))
	counter("skylab_management_terms_run_failures_total", s.failures)
	out.WriteString("# TYPE skylab_management_terms_route_total counter\n")
	for _, item := range []struct {
		event string
		value int64
	}{{"started", s.hook.started}, {"ended", s.hook.ended}, {"undated", s.hook.undated}, {"error", s.hook.errors}, {"precheck_failed", s.hook.precheck}} {
		fmt.Fprintf(&out, "skylab_management_terms_route_total{event=%q} %d\n", item.event, item.value)
	}
	out.WriteString("# TYPE skylab_management_term_mail_total counter\n")
	for _, outcome := range outcomeNames {
		fmt.Fprintf(&out, "skylab_management_term_mail_total{outcome=%q} %d\n", outcome, s.outcomes[outcome])
	}
	gauge("skylab_management_term_mail_backlog", int64(s.backlog))
	gauge("skylab_management_term_mail_oldest_age_seconds", int64(s.oldest/time.Second))
	return out.String()
}
