package event

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// FeedbackMode is where an Event asks for feedback (etkinlik geri bildirim
// formu, Yusuf's decisions K1 of 2026-10-20): one form for the Event, a
// form for each Session, or one form that asks about each Session.
type FeedbackMode string

const (
	// FeedbackEvent asks with the Event's feedback form, once (the default).
	FeedbackEvent FeedbackMode = "event"
	// FeedbackPerSession asks with each Session's own form; the Event's
	// stays optional, for the Event as a whole.
	FeedbackPerSession FeedbackMode = "per_session"
	// FeedbackEventWithSessions asks with the Event's form, which asks about
	// each Session itself (the form's own structure: core keeps none of it).
	FeedbackEventWithSessions FeedbackMode = "event_with_sessions"
)

// Valid reports whether m is one of the three modes.
func (m FeedbackMode) Valid() bool {
	return m == FeedbackEvent || m == FeedbackPerSession || m == FeedbackEventWithSessions
}

// feedbackMode is m, the default for none.
func feedbackMode(m FeedbackMode) FeedbackMode {
	if m == "" {
		return FeedbackEvent
	}
	return m
}

// ErrFormInUse refuses a form already somewhere else (problem code
// form_in_use): a feedback form is in one place only, never also an
// application or extra form, nor another Event's feedback form.
// Application and extra forms may still be shared between Events, as
// before.
var ErrFormInUse = fmt.Errorf("event: form in use: %w", ErrConflict)

// FeedbackFormID is the Event's feedback form, false when it has none.
func (e Event) FeedbackFormID() (uuid.UUID, bool) {
	if e.FeedbackFormURL == "" {
		return uuid.Nil, false
	}
	return FormIDFromURL(e.FeedbackFormURL)
}

// HasFeedbackForm reports whether formID is the Event's feedback form.
func (e Event) HasFeedbackForm(formID uuid.UUID) bool {
	id, ok := e.FeedbackFormID()
	return ok && id == formID
}

// feedbackLink is the Event's feedback form as its short link is synced:
// managed by the Event, labelled "<Event> · Geri bildirim".
func feedbackLink(e Event) (FormLink, bool) {
	id, ok := e.FeedbackFormID()
	if !ok {
		return FormLink{}, false
	}
	return FormLink{FormID: id, URL: e.FeedbackFormURL, Alias: e.FeedbackFormAlias, Label: e.Name + " · Geri bildirim"}, true
}

// checkFormPlaces applies the one-place rule to e as it is about to be
// written: its feedback form may not be one of its own application or extra
// forms, nor any form of another current Event; and none of its application
// or extra forms may be another current Event's feedback form. An archived
// Event released its forms and holds none.
func (s *service) checkFormPlaces(ctx context.Context, e Event) error {
	feedback, hasFeedback := e.FeedbackFormID()
	if hasFeedback && e.ListsForm(feedback) {
		return ErrFormInUse
	}
	applications := applicationLinksOf(e)
	if !hasFeedback && len(applications) == 0 {
		return nil
	}
	others, err := s.store.ListIncludingDrafts(ctx, "")
	if err != nil {
		return err
	}
	for _, other := range others {
		if other.ID == e.ID {
			continue
		}
		if hasFeedback && (other.ListsForm(feedback) || other.HasFeedbackForm(feedback)) {
			return ErrFormInUse
		}
		for _, link := range applications {
			if other.HasFeedbackForm(link.FormID) {
				return ErrFormInUse
			}
		}
	}
	return nil
}

// formGuard is the one-place rule a write applies again inside its own
// transaction: the store takes a transaction-scoped advisory lock on each
// of ids (in order) and runs check before it writes. Two writes that name
// the same form are thus decided one after the other, the second seeing the
// first committed (decision of the 2026-10-20 review, B1).
type formGuard struct {
	ids   []uuid.UUID
	check func(ctx context.Context) error
}

// guardForms sets e's guard: its application, extra and feedback forms,
// checked as checkFormPlaces does.
func (s *service) guardForms(e *Event) {
	ids := make([]uuid.UUID, 0, len(e.ExtraFormURLs)+2)
	for _, link := range applicationLinksOf(*e) {
		ids = append(ids, link.FormID)
	}
	if id, ok := e.FeedbackFormID(); ok {
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		e.formGuard = nil
		return
	}
	checked := *e
	checked.formGuard = nil
	e.formGuard = &formGuard{ids: ids, check: func(ctx context.Context) error {
		return s.checkFormPlaces(ctx, checked)
	}}
}

// lockFormPlaces takes the guard's locks inside tx, in a fixed order, and
// applies its check; a nil guard does nothing.
func lockFormPlaces(ctx context.Context, tx pgx.Tx, g *formGuard) error {
	if g == nil {
		return nil
	}
	ids := slices.Clone(g.ids)
	slices.SortFunc(ids, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	ids = slices.Compact(ids)
	for _, id := range ids {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('form-place:' || $1::text, 0))`, id); err != nil {
			return err
		}
	}
	return g.check(ctx)
}
