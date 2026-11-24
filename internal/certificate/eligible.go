package certificate

import "github.com/skylab-kulubu/core-backend/internal/event"

const (
	RuleNone  = event.RuleNone
	RuleOnce  = event.RuleOnce
	RuleCount = event.RuleCount
	RuleRatio = event.RuleRatio
)

// Rule is an Event's certificate rule (docs/certificate-rule.md): its kind,
// the count rule's threshold (nil for the default) and the ratio rule's
// ratio.
type Rule struct {
	Kind     string
	MinCount *int
	Ratio    *float64
}

// RuleOf is the Event's certificate rule.
func RuleOf(e event.Event) Rule {
	return Rule{Kind: e.AttendanceRule, MinCount: e.AttendanceMinCount, Ratio: e.AttendanceRatio}
}

// EffectiveMin is the count rule's threshold over counted Sessions: nil is
// ceil(3N/4) in integers ((3N+3)/4), else min(M, N); N = 0 is 0.
func EffectiveMin(minCount *int, counted int) int {
	return event.EffectiveMin(minCount, counted)
}

// Eligible decides a Ticket: attended is its check-ins on counted live
// Sessions, counted the number of those Sessions (N), missingRequired the
// required live Sessions it has no check-in on. No counted Session, a
// missing required Session, and the none rule never issue. The ratio rule
// keeps its float comparison of before the count rule.
func Eligible(r Rule, attended, counted, missingRequired int) bool {
	if counted <= 0 || missingRequired > 0 {
		return false
	}
	switch r.Kind {
	case RuleOnce:
		return attended >= 1
	case RuleCount:
		return attended >= EffectiveMin(r.MinCount, counted)
	case RuleRatio:
		ratio := 0.0
		if r.Ratio != nil {
			ratio = *r.Ratio
		}
		return float64(attended)/float64(counted) >= ratio
	default:
		return false
	}
}
