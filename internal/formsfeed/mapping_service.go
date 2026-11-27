package formsfeed

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
)

// Refusals of the form mapping routes (the handler's problem codes).
var (
	// ErrMappingEventNotFound is no such current Event, or a draft the
	// caller may not edit (404).
	ErrMappingEventNotFound = errors.New("formsfeed: no such current Event")
	// ErrMappingForbidden is a caller who may not edit the Event, or a
	// product's service account (403).
	ErrMappingForbidden = errors.New("formsfeed: may not edit the Event")
	// ErrMappingNotListed is a form the Event does not list as its
	// application or an extra form (404 form_not_listed).
	ErrMappingNotListed = errors.New("formsfeed: the Event does not list the form")
	// ErrFormsPullOff is EVENTS_FORMS_ENABLED off: the questions are not
	// read (503 forms_pull_off).
	ErrFormsPullOff = errors.New("formsfeed: Forms' integration is off")
	// ErrFormsUnavailable is a Forms core cannot read: no internal address
	// or service account, Forms down, or a refusal (503
	// forms_unavailable).
	ErrFormsUnavailable = errors.New("formsfeed: Forms could not be read")
	// ErrFormGone is a form Forms does not have (404 form_not_found):
	// deleted, or a Forms without the route yet.
	ErrFormGone = errors.New("formsfeed: Forms has no such form")
	// ErrFormsAccessDenied is a caller who is not on the form's team in
	// Forms (owner, editor or viewer; Form.Sees), or a Forms that does not
	// say who is (403 forms_access_denied).
	ErrFormsAccessDenied = errors.New("formsfeed: not the form's owner or collaborator in Forms")
)

// FormReader reads a form's questions from Forms (Client).
type FormReader interface {
	Form(ctx context.Context, formID uuid.UUID) (Form, error)
}

// Mappings serves an Event's form mappings (docs/form-mappings.md): the
// form's questions for the panel, and the mapping itself. Every call is a
// person's who may edit the Event (authz Update on the Event, its
// Co-organizers included), and who is on the form's team in Forms: its
// owner, an editor or a viewer (read from Forms each time; Form.Sees); a
// product's service account never is.
type Mappings struct {
	Events BackfillEvents
	Authz  authz.Authorizer
	Store  MappingStore
	// Forms nil is a core without Forms' internal address or its service
	// account: every call is ErrFormsUnavailable.
	Forms FormReader
	// On is EVENTS_FORMS_ENABLED: off, every call is ErrFormsPullOff (each
	// reads Forms).
	On bool
	// Collects queues the form's gathering when its mapping is first
	// saved, so the answers that named nobody before are kept with the
	// guests it builds (docs/form-respondents.md); nil queues none.
	Collects *Collects
	Now      func() time.Time
}

func (m *Mappings) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// editable reads the Event and checks the caller may edit it and that it
// lists the form.
func (m *Mappings) editable(ctx context.Context, p authz.Principal, eventID, formID uuid.UUID) (event.Event, error) {
	ev, err := m.Events.Get(ctx, eventID)
	if errors.Is(err, event.ErrNotFound) {
		return event.Event{}, ErrMappingEventNotFound
	}
	if err != nil {
		return event.Event{}, err
	}
	if ev.ArchivedAt != nil {
		return event.Event{}, ErrMappingEventNotFound
	}
	if p.ServiceAccount || !m.Authz.Allow(p, event.AuthzResource(ev, authz.TypeEvent, m.now()), authz.Update) {
		if ev.Status == event.StatusDraft {
			// A draft is shown to its editors only (ADR 0066).
			return event.Event{}, ErrMappingEventNotFound
		}
		return event.Event{}, ErrMappingForbidden
	}
	if !ev.ListsForm(formID) {
		return event.Event{}, ErrMappingNotListed
	}
	return ev, nil
}

// readForm reads the form from Forms, its errors as the routes answer them,
// and checks the caller is on its team there (Form.Sees).
func (m *Mappings) readForm(ctx context.Context, p authz.Principal, formID uuid.UUID) (Form, error) {
	if !m.On {
		return Form{}, ErrFormsPullOff
	}
	if m.Forms == nil {
		return Form{}, ErrFormsUnavailable
	}
	form, err := m.Forms.Form(ctx, formID)
	switch {
	case errors.Is(err, ErrNotFound):
		return Form{}, ErrFormGone
	case err != nil:
		// The cause (403, 5xx, a broken answer) is the log's, not the
		// panel's; it never carries an answer.
		return Form{}, errors.Join(ErrFormsUnavailable, err)
	}
	if !form.Sees(p.ID) {
		return Form{}, ErrFormsAccessDenied
	}
	return form, nil
}

// FormQuestions is the questions route's answer: the form's title and
// questions, and the mapping its identity fields suggest (Suggest, K5).
type FormQuestions struct {
	Form
	Suggestion Mapping
}

// Questions reads the form's questions for the panel's mapping.
func (m *Mappings) Questions(ctx context.Context, p authz.Principal, eventID, formID uuid.UUID) (FormQuestions, error) {
	if _, err := m.editable(ctx, p, eventID, formID); err != nil {
		return FormQuestions{}, err
	}
	form, err := m.readForm(ctx, p, formID)
	if err != nil {
		return FormQuestions{}, err
	}
	return FormQuestions{Form: form, Suggestion: Suggest(form.Questions)}, nil
}

// Get answers the saved mapping, or ErrMappingNotFound.
func (m *Mappings) Get(ctx context.Context, p authz.Principal, eventID, formID uuid.UUID) (Mapping, error) {
	if _, err := m.editable(ctx, p, eventID, formID); err != nil {
		return Mapping{}, err
	}
	if _, err := m.readForm(ctx, p, formID); err != nil {
		return Mapping{}, err
	}
	return m.Store.Get(ctx, eventID, formID)
}

// Put checks and saves the mapping: ErrMappingInvalid for one that breaks
// the rule, a *MappingQuestionMissingError for ids the form does not have
// (read from Forms now), ErrMappingTooManyQuestions for one that takes the
// form's mappings past MaxFields questions, ErrFormsPullOff with the switch off. created is true when there was none (the first
// save, which bilet 04 may hang the backfill on).
func (m *Mappings) Put(ctx context.Context, p authz.Principal, in Mapping) (saved Mapping, created bool, err error) {
	ev, err := m.editable(ctx, p, in.EventID, in.FormID)
	if err != nil {
		return Mapping{}, false, err
	}
	defer func() {
		if err == nil && created {
			m.Collects.FormsLinked(ctx, ev, []uuid.UUID{in.FormID})
		}
	}()
	in = in.Normalized()
	if err := in.Validate(); err != nil {
		return Mapping{}, false, err
	}
	form, err := m.readForm(ctx, p, in.FormID)
	if err != nil {
		return Mapping{}, false, err
	}
	if missing := in.MissingFrom(form.Questions); len(missing) > 0 {
		return Mapping{}, false, &MappingQuestionMissingError{Missing: missing}
	}
	// Every Event's mapping of the form is read in one request to Forms.
	all, err := m.Store.ForForm(ctx, in.FormID)
	if err != nil {
		return Mapping{}, false, err
	}
	together := []Mapping{in}
	for _, other := range all {
		if other.EventID != in.EventID {
			together = append(together, other)
		}
	}
	if len(MappedFields(together)) > MaxFields {
		return Mapping{}, false, ErrMappingTooManyQuestions
	}
	in.UpdatedBy = nil
	if id, err := uuid.Parse(p.ID); err == nil {
		in.UpdatedBy = &id
	}
	in.UpdatedAt = m.now().UTC()
	return m.Store.Put(ctx, in)
}

// Delete removes the saved mapping; ErrMappingNotFound when there was none.
// A form Forms no longer has (ErrFormGone) may lose its mapping too.
func (m *Mappings) Delete(ctx context.Context, p authz.Principal, eventID, formID uuid.UUID) error {
	if _, err := m.editable(ctx, p, eventID, formID); err != nil {
		return err
	}
	if _, err := m.readForm(ctx, p, formID); err != nil && !errors.Is(err, ErrFormGone) {
		return err
	}
	deleted, err := m.Store.Delete(ctx, eventID, formID)
	if err == nil && !deleted {
		return ErrMappingNotFound
	}
	return err
}
