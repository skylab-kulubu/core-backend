package event

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/optional"
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
// form_in_use): a form is in one place only. A feedback form is never also
// an application or extra form, nor another Event's feedback form; an
// application or extra form is on one current Event only (ADR 0069,
// addendum of 2026-11-22). A refusal is a *FormInUseError, which says where.
var ErrFormInUse = fmt.Errorf("event: form in use: %w", ErrConflict)

// FormInUseError is ErrFormInUse with the current Event that holds the
// form: Holder is nil when the caller may not see that Event (a draft of
// another team), and the refusal then names no place.
type FormInUseError struct {
	FormID uuid.UUID
	Holder *FormHolder
}

// FormHolder is the current Event a refused form is on, as much as the
// 409 tells.
type FormHolder struct {
	ID        uuid.UUID
	Name      string
	Status    Status
	OwnerTeam string
}

func (e *FormInUseError) Error() string { return ErrFormInUse.Error() }

func (e *FormInUseError) Unwrap() error { return ErrFormInUse }

// shownTo is err with the holder of a refused form left out when p may not
// see it (event.VisibleTo): who may not see a draft is not told it exists.
func (s *service) shownTo(ctx context.Context, p *authz.Principal, err error) error {
	var inUse *FormInUseError
	if !errors.As(err, &inUse) || inUse.Holder == nil {
		return err
	}
	// The holder as stored, its Co-organizers included; the 409's summary
	// when it cannot be read.
	holder := Event{Status: inUse.Holder.Status, OwnerTeam: inUse.Holder.OwnerTeam}
	if stored, getErr := s.store.GetIncludingArchived(ctx, inUse.Holder.ID); getErr == nil {
		holder = stored
	}
	if s.VisibleTo(p, holder) {
		return err
	}
	return &FormInUseError{FormID: inUse.FormID}
}

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

// SessionFeedbackForm is a Session's feedback form, where the one-place
// rule, the short links and the forms report find it.
type SessionFeedbackForm struct {
	EventID   uuid.UUID
	SessionID uuid.UUID
	Title     string
	URL       string
	Alias     string
}

// formPlaces is where the forms are now: the current Events (drafts too)
// and the current Sessions' feedback forms.
type formPlaces struct {
	events   []Event
	sessions []SessionFeedbackForm
}

func (s *service) currentFormPlaces(ctx context.Context) (formPlaces, error) {
	events, err := s.store.ListIncludingDrafts(ctx, "")
	if err != nil {
		return formPlaces{}, err
	}
	sessions, err := s.store.ListSessionFeedbackForms(ctx, uuid.Nil)
	if err != nil {
		return formPlaces{}, err
	}
	return formPlaces{events: events, sessions: sessions}, nil
}

// A place lookup answers the Event that holds the form (for a Session's
// feedback form, the Session's Event) and whether one does.

// holdsAnywhere is whether formID is any form of an Event other than
// exceptEvent, or the feedback form of a Session other than exceptSession:
// where a feedback form may not be too.
func (fp formPlaces) holdsAnywhere(formID, exceptEvent, exceptSession uuid.UUID) (uuid.UUID, bool) {
	for _, e := range fp.events {
		if e.ID != exceptEvent && (e.ListsForm(formID) || e.HasFeedbackForm(formID)) {
			return e.ID, true
		}
	}
	return fp.holdsAsSessionFeedback(formID, exceptSession)
}

// holdsAsFeedback is whether formID is the feedback form of an Event other
// than exceptEvent or of any Session: where an application form may not be.
func (fp formPlaces) holdsAsFeedback(formID, exceptEvent uuid.UUID) (uuid.UUID, bool) {
	for _, e := range fp.events {
		if e.ID != exceptEvent && e.HasFeedbackForm(formID) {
			return e.ID, true
		}
	}
	return fp.holdsAsSessionFeedback(formID, uuid.Nil)
}

// holdsAsApplication is whether formID is an application or extra form of
// an Event other than exceptEvent: an application form is on one current
// Event only, a draft or an ended one too (ADR 0069, addendum of
// 2026-11-22).
func (fp formPlaces) holdsAsApplication(formID, exceptEvent uuid.UUID) (uuid.UUID, bool) {
	for _, e := range fp.events {
		if e.ID != exceptEvent && e.ListsForm(formID) {
			return e.ID, true
		}
	}
	return uuid.Nil, false
}

func (fp formPlaces) holdsAsSessionFeedback(formID, exceptSession uuid.UUID) (uuid.UUID, bool) {
	for _, f := range fp.sessions {
		if id, ok := FormIDFromURL(f.URL); ok && id == formID && f.SessionID != exceptSession {
			return f.EventID, true
		}
	}
	return uuid.Nil, false
}

// inUse refuses formID, naming the current Event holderID when it is one
// of fp's.
func (fp formPlaces) inUse(formID, holderID uuid.UUID) error {
	refused := &FormInUseError{FormID: formID}
	for _, e := range fp.events {
		if e.ID == holderID {
			refused.Holder = &FormHolder{ID: e.ID, Name: e.Name, Status: e.Status, OwnerTeam: e.OwnerTeam}
			break
		}
	}
	return refused
}

// checkFormPlaces applies the one-place rule to e as it is about to be
// written: its feedback form may not be one of its own application or extra
// forms, nor any form of another current Event, nor a Session's feedback
// form; none of its application or extra forms may be another current
// Event's or any Session's feedback form; and none may be another current
// Event's application or extra form, unless kept lists it: the forms the
// stored Event already lists when it is updated. An Event that shared one
// before the rule (ADR 0069, addendum of 2026-11-22) keeps it and is
// written as before; only a new binding is refused. With sessions
// (restoring the Event) its Sessions' feedback forms are held to it too.
// An archived Event released its forms and holds none.
func (s *service) checkFormPlaces(ctx context.Context, e Event, sessions bool, kept map[uuid.UUID]bool) error {
	feedback, hasFeedback := e.FeedbackFormID()
	if hasFeedback && e.ListsForm(feedback) {
		return &FormInUseError{FormID: feedback}
	}
	applications := applicationLinksOf(e)
	var own []SessionFeedbackForm
	if sessions {
		var err error
		if own, err = s.store.ListSessionFeedbackForms(ctx, e.ID); err != nil {
			return err
		}
	}
	if !hasFeedback && len(applications) == 0 && len(own) == 0 {
		return nil
	}
	places, err := s.currentFormPlaces(ctx)
	if err != nil {
		return err
	}
	if hasFeedback {
		if holder, ok := places.holdsAnywhere(feedback, e.ID, uuid.Nil); ok {
			return places.inUse(feedback, holder)
		}
	}
	for _, link := range applications {
		if holder, ok := places.holdsAsFeedback(link.FormID, e.ID); ok {
			return places.inUse(link.FormID, holder)
		}
		if kept[link.FormID] {
			continue
		}
		if holder, ok := places.holdsAsApplication(link.FormID, e.ID); ok {
			return places.inUse(link.FormID, holder)
		}
	}
	for _, f := range own {
		if id, ok := FormIDFromURL(f.URL); ok {
			if holder, ok := places.holdsAnywhere(id, e.ID, f.SessionID); ok {
				return places.inUse(id, holder)
			}
		}
	}
	return nil
}

// listedForms are the application and extra forms e lists: those an update
// of e keeps although another current Event lists them too.
func listedForms(e Event) map[uuid.UUID]bool {
	listed := map[uuid.UUID]bool{}
	for _, link := range applicationLinksOf(e) {
		listed[link.FormID] = true
	}
	return listed
}

// checkSessionFormPlace applies the one-place rule to a Session's feedback
// form: it may not be any form of a current Event, its own Event's
// included, nor another Session's feedback form.
func (s *service) checkSessionFormPlace(ctx context.Context, sess Session) error {
	formID, ok := FormIDFromURL(sess.FeedbackFormURL)
	if sess.FeedbackFormURL == "" || !ok {
		return nil
	}
	places, err := s.currentFormPlaces(ctx)
	if err != nil {
		return err
	}
	if holder, ok := places.holdsAnywhere(formID, uuid.Nil, sess.ID); ok {
		return places.inUse(formID, holder)
	}
	return nil
}

// syncEventForms syncs the short links of e's forms: its application,
// extra and feedback forms and its Sessions' feedback forms. When the
// Sessions' forms cannot be read it syncs nothing (a sync without them
// would release their links); like every sync, it never fails the write.
func (s *service) syncEventForms(ctx context.Context, e Event) {
	if s.formLinks == nil {
		return
	}
	sessions, err := s.store.ListSessionFeedbackForms(ctx, e.ID)
	if err != nil {
		log.Printf("event %s: read session feedback forms: %v", e.ID, err)
		return
	}
	links := formLinksOf(e)
	for _, f := range sessions {
		if id, ok := FormIDFromURL(f.URL); ok {
			links = append(links, FormLink{FormID: id, URL: f.URL, Alias: f.Alias, Label: e.Name + " · " + f.Title + " · Geri bildirim"})
		}
	}
	s.syncFormLinks(ctx, e.ID, links)
}

// syncEventFormsOf syncs the current Event eventID's form links (an
// archived one holds none).
func (s *service) syncEventFormsOf(ctx context.Context, eventID uuid.UUID) {
	e, err := s.store.Get(ctx, eventID)
	if err != nil {
		if !errors.Is(err, ErrNotFound) {
			log.Printf("event %s: sync form links: %v", eventID, err)
		}
		return
	}
	s.syncEventForms(ctx, e)
}

// SessionFeedback is what a write of a Session sends of its feedback
// form: a field left out keeps the stored value, so a client that sends
// the Session without them (sky-app, the panel before ticket 03) keeps
// them. null or "" clears the form or the alias; feedbackAutoSend may not
// be null.
type SessionFeedback struct {
	FormURL   optional.Field[string] `json:"feedbackFormUrl"`
	FormAlias optional.Field[string] `json:"feedbackFormAlias"`
	AutoSend  optional.Field[bool]   `json:"feedbackAutoSend"`
}

func (f SessionFeedback) apply(sess, existing Session) (Session, error) {
	if f.AutoSend.Null {
		return Session{}, ErrInvalid
	}
	sess.FeedbackFormURL = strings.TrimSpace(f.FormURL.Or(existing.FeedbackFormURL))
	sess.FeedbackFormAlias = strings.TrimSpace(f.FormAlias.Or(existing.FeedbackFormAlias))
	sess.FeedbackAutoSend = f.AutoSend.Or(existing.FeedbackAutoSend)
	return sess, nil
}

// WithFeedback is sess with what f sends of its feedback form.
func (sess Session) WithFeedback(f SessionFeedback) (Session, error) {
	return f.apply(sess, sess)
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
// checked as checkFormPlaces does with kept.
func (s *service) guardForms(e *Event, kept map[uuid.UUID]bool) {
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
		return s.checkFormPlaces(ctx, checked, false, kept)
	}}
}

// guardRestore is the guard of restoring e: its application, extra and
// feedback forms and its Sessions' feedback forms, checked as Restore's
// checkFormPlaces does (with the Sessions, nothing kept). A restore and a
// write that binds one of them at once are thus decided one after the other.
func (s *service) guardRestore(ctx context.Context, e Event) (*formGuard, error) {
	ids := make([]uuid.UUID, 0, len(e.ExtraFormURLs)+2)
	for _, link := range applicationLinksOf(e) {
		ids = append(ids, link.FormID)
	}
	if id, ok := e.FeedbackFormID(); ok {
		ids = append(ids, id)
	}
	own, err := s.store.ListSessionFeedbackForms(ctx, e.ID)
	if err != nil {
		return nil, err
	}
	for _, f := range own {
		if id, ok := FormIDFromURL(f.URL); ok {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	checked := e
	checked.formGuard = nil
	return &formGuard{ids: ids, check: func(ctx context.Context) error {
		return s.checkFormPlaces(ctx, checked, true, nil)
	}}, nil
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

// guardSessionForm sets sess's guard: its feedback form, checked as
// checkSessionFormPlace does.
func (s *service) guardSessionForm(sess *Session) {
	sess.formGuard = nil
	id, ok := FormIDFromURL(sess.FeedbackFormURL)
	if sess.FeedbackFormURL == "" || !ok {
		return
	}
	checked := *sess
	sess.formGuard = &formGuard{ids: []uuid.UUID{id}, check: func(ctx context.Context) error {
		return s.checkSessionFormPlace(ctx, checked)
	}}
}
