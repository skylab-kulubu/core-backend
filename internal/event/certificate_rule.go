package event

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/optional"
)

// The certificate rule kinds (events.attendance_rule;
// docs/certificate-rule.md). RuleRatio is the rule of before the count rule:
// an Event that has it keeps it, a new write may not choose it.
const (
	RuleNone  = "none"
	RuleOnce  = "once"
	RuleCount = "count"
	RuleRatio = "ratio"
)

// sessionTypesNotCounted are the Session types a new Session is created not
// counted for, when the body does not say (open question S1 of the
// certificate × session spec; one place to change it). Compared in upper
// case, as the panel writes the types.
var sessionTypesNotCounted = map[string]bool{
	"NETWORKING": true,
}

// CountedByDefault is whether a new Session of the type is counted when the
// body does not say.
func CountedByDefault(sessionType string) bool {
	return !sessionTypesNotCounted[strings.ToUpper(strings.TrimSpace(sessionType))]
}

// EffectiveMin is the count rule's threshold over counted Sessions: the
// default ceil(3N/4) when minCount is nil (in integers, no float), else
// min(M, N), so that cancelling or uncounting Sessions never makes the rule
// impossible. N = 0 is 0.
func EffectiveMin(minCount *int, counted int) int {
	if counted <= 0 {
		return 0
	}
	if minCount == nil || *minCount < 1 {
		return (3*counted + 3) / 4
	}
	return min(*minCount, counted)
}

// ratioMin is the fewest check-ins the ratio rule takes over counted
// Sessions, decided with the rule's own float comparison.
func ratioMin(ratio float64, counted int) int {
	for k := 0; k <= counted; k++ {
		if float64(k)/float64(counted) >= ratio {
			return k
		}
	}
	return counted + 1
}

// RuleMin is the fewest counted check-ins the Event's rule takes: 0 for no
// certificate or no counted Session.
func RuleMin(e Event, counted int) int {
	if counted <= 0 {
		return 0
	}
	switch e.AttendanceRule {
	case RuleOnce:
		return 1
	case RuleCount:
		return EffectiveMin(e.AttendanceMinCount, counted)
	case RuleRatio:
		ratio := 0.0
		if e.AttendanceRatio != nil {
			ratio = *e.AttendanceRatio
		}
		return ratioMin(ratio, counted)
	default:
		return 0
	}
}

// SessionCertificate is what a Session body sends of its certificate marks:
// one left out keeps the stored mark, or on create takes the default
// (counted unless the type is CountedByDefault's exception; not required).
type SessionCertificate struct {
	Counted  optional.Field[bool] `json:"certificateCounted"`
	Required optional.Field[bool] `json:"certificateRequired"`
}

// WithCertificate is sess with what c sends of its certificate marks.
func (sess Session) WithCertificate(c SessionCertificate) Session {
	sess.certificate = c
	return sess
}

// certificateMarks sets sess's marks from what its body sent over kept (the
// stored marks; on create, the defaults): ErrInvalid for a null, or for a
// required Session that is not counted.
func (sess Session) certificateMarks(counted, required bool) (Session, error) {
	c := sess.certificate
	if c.Counted.Null || c.Required.Null {
		return Session{}, ErrInvalid
	}
	sess.CertificateCounted = c.Counted.Or(counted)
	sess.CertificateRequired = c.Required.Or(required)
	if sess.CertificateRequired && !sess.CertificateCounted {
		return Session{}, ErrInvalid
	}
	return sess, nil
}

// SessionCertificateFlags are one Session's marks as the certificate rule
// writes them.
type SessionCertificateFlags struct {
	ID       uuid.UUID `json:"id"`
	Counted  bool      `json:"counted"`
	Required bool      `json:"required"`
}

// CertificateRuleInput is the body of PUT /v1/events/{id}/certificate-rule:
// the rule (none, once or count; ratio is refused), the count rule's
// threshold (null for the default; ignored by the other rules) and the marks
// of the Sessions it lists. A Session it does not list keeps its marks.
type CertificateRuleInput struct {
	Rule     string
	MinCount *int
	Sessions []SessionCertificateFlags
}

// CertificateRuleSession is one of the Event's current Sessions as the rule
// editor shows it: cancelled ones too, which are not counted whatever their
// mark says.
type CertificateRuleSession struct {
	ID          uuid.UUID  `json:"id"`
	DayID       uuid.UUID  `json:"dayId"`
	Title       string     `json:"title"`
	SessionType string     `json:"sessionType"`
	StartTime   *time.Time `json:"startTime,omitempty"`
	EndTime     *time.Time `json:"endTime,omitempty"`
	Cancelled   bool       `json:"cancelled"`
	Counted     bool       `json:"counted"`
	Required    bool       `json:"required"`
}

// CertificateRule is GET /v1/events/{id}/certificate-rule: the rule, its
// threshold as stored (MinCount) and as it applies now (EffectiveMin, over
// CountedTotal, the live counted Sessions), and the Sessions.
type CertificateRule struct {
	Rule         string                   `json:"rule"`
	Ratio        *float64                 `json:"ratio,omitempty"`
	MinCount     *int                     `json:"minCount"`
	EffectiveMin int                      `json:"effectiveMin"`
	CountedTotal int                      `json:"countedTotal"`
	Sessions     []CertificateRuleSession `json:"sessions"`
}

// mayReadCertificateRule is whether p may read the Event's rule: who may
// edit the Event, or read its certificates' workspace.
func (s *service) mayReadCertificateRule(p authz.Principal, ownerTeam string) bool {
	return s.authz.Allow(p, resource(ownerTeam), authz.Update) ||
		s.authz.Allow(p, authz.Resource{Type: authz.TypeCertificate, OwnerTeam: ownerTeam}, authz.Read) ||
		s.authz.Allow(p, authz.Resource{Type: authz.TypeCertificateTemplate, OwnerTeam: ownerTeam}, authz.Read)
}

// CertificateRule reads the Event's certificate rule and its current
// Sessions (on current days), in the program's order.
func (s *service) CertificateRule(ctx context.Context, p authz.Principal, eventID uuid.UUID) (CertificateRule, error) {
	e, err := s.visible(ctx, &p, eventID)
	if err != nil {
		return CertificateRule{}, err
	}
	if !s.mayReadCertificateRule(p, e.OwnerTeam) {
		return CertificateRule{}, ErrForbidden
	}
	return s.certificateRule(ctx, e)
}

func (s *service) certificateRule(ctx context.Context, e Event) (CertificateRule, error) {
	days, err := s.store.ListDays(ctx, e.ID)
	if err != nil {
		return CertificateRule{}, err
	}
	out := CertificateRule{
		Rule: attendanceRule(e.AttendanceRule), Ratio: e.AttendanceRatio, MinCount: e.AttendanceMinCount,
		Sessions: []CertificateRuleSession{},
	}
	for _, day := range days {
		sessions, err := s.store.ListSessions(ctx, day.ID)
		if err != nil {
			return CertificateRule{}, err
		}
		for _, sess := range sessions {
			out.Sessions = append(out.Sessions, CertificateRuleSession{
				ID: sess.ID, DayID: day.ID, Title: sess.Title, SessionType: sess.SessionType,
				StartTime: sess.StartTime, EndTime: sess.EndTime, Cancelled: sess.Cancelled,
				Counted: sess.CertificateCounted, Required: sess.CertificateRequired,
			})
			if !sess.Cancelled && sess.CertificateCounted {
				out.CountedTotal++
			}
		}
	}
	if out.Rule != RuleCount {
		out.MinCount = nil
	}
	out.EffectiveMin = RuleMin(Event{AttendanceRule: out.Rule, AttendanceRatio: e.AttendanceRatio, AttendanceMinCount: out.MinCount}, out.CountedTotal)
	return out, nil
}

// SetCertificateRule writes the Event's rule and the listed Sessions' marks
// in one transaction (all or nothing), for whoever may edit the Event. A
// ratio rule, a threshold below 1, a Session listed twice, a required one
// that is not counted, and a Session that is not one of the Event's current
// Sessions are ErrInvalid. A threshold above N is kept: it applies as N.
func (s *service) SetCertificateRule(ctx context.Context, p authz.Principal, eventID uuid.UUID, in CertificateRuleInput) (CertificateRule, error) {
	e, err := s.visible(ctx, &p, eventID)
	if err != nil {
		return CertificateRule{}, err
	}
	if !s.authz.Allow(p, resource(e.OwnerTeam), authz.Update) {
		return CertificateRule{}, ErrForbidden
	}
	rule := strings.ToLower(strings.TrimSpace(in.Rule))
	switch rule {
	case RuleNone, RuleOnce:
		in.MinCount = nil
	case RuleCount:
		if in.MinCount != nil && *in.MinCount < 1 {
			return CertificateRule{}, ErrInvalid
		}
	default:
		return CertificateRule{}, ErrInvalid
	}
	seen := make(map[uuid.UUID]bool, len(in.Sessions))
	for _, f := range in.Sessions {
		if f.ID == uuid.Nil || seen[f.ID] || (f.Required && !f.Counted) {
			return CertificateRule{}, ErrInvalid
		}
		seen[f.ID] = true
	}
	if err := s.store.SetCertificateRule(ctx, e.ID, rule, in.MinCount, in.Sessions); err != nil {
		return CertificateRule{}, err
	}
	written, err := s.store.Get(ctx, e.ID)
	if err != nil {
		return CertificateRule{}, err
	}
	return s.certificateRule(ctx, written)
}

// normalizeAttendance checks and cleans the Event's rule as a write sends
// it. existing is the stored Event (nil on create): the ratio rule is kept
// only as it is stored, unchanged; a new write may not choose it or change
// its ratio. The threshold belongs to the count rule alone: any other rule
// writes none.
func normalizeAttendance(in Event, existing *Event) (Event, error) {
	rule := strings.TrimSpace(strings.ToLower(in.AttendanceRule))
	if rule == "" {
		rule = RuleNone
	}
	in.AttendanceRule = rule
	switch rule {
	case RuleNone, RuleOnce:
		in.AttendanceMinCount = nil
		return in, nil
	case RuleCount:
		if in.AttendanceMinCount != nil && *in.AttendanceMinCount < 1 {
			return Event{}, ErrInvalid
		}
		in.AttendanceRatio = nil
		return in, nil
	case RuleRatio:
		if existing == nil || existing.AttendanceRule != RuleRatio || !sameRatio(existing.AttendanceRatio, in.AttendanceRatio) {
			return Event{}, ErrInvalid
		}
		if in.AttendanceRatio == nil || *in.AttendanceRatio <= 0 || *in.AttendanceRatio > 1 {
			return Event{}, ErrInvalid
		}
		in.AttendanceMinCount = nil
		return in, nil
	default:
		return Event{}, ErrInvalid
	}
}

func sameRatio(a, b *float64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
