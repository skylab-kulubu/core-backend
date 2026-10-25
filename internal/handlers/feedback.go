package handlers

import (
	"context"
	"errors"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/feedback"
)

// FeedbackCounter reads an Event's feedback invite counts.
type FeedbackCounter interface {
	Counts(ctx context.Context, eventID uuid.UUID) (feedback.Counts, error)
}

// FeedbackSender mails a target's feedback form at once ("Şimdi gönder",
// ticket 06; feedback.PostgresStore).
type FeedbackSender interface {
	SendNow(ctx context.Context, t feedback.Target, now time.Time) (sent bool, recipients int, err error)
}

// FeedbackReminders cancels the feedback reminders of a form the panel
// closed (ticket 07; feedback.PostgresStore).
type FeedbackReminders interface {
	CancelReminders(ctx context.Context, t feedback.Target, at time.Time) (int, error)
}

// FeedbackHandler answers an Event's feedback counts (etkinlik geri bildirim
// formu, ticket 04), sends a form at once (ticket 06) and cancels the
// reminders of a closed form (ticket 07).
type FeedbackHandler struct {
	events    event.Service
	authz     authz.Authorizer
	counts    FeedbackCounter
	sender    FeedbackSender
	reminders FeedbackReminders
}

// RemindWith gives the handler the reminders' cancelling.
func (h *FeedbackHandler) RemindWith(reminders FeedbackReminders) *FeedbackHandler {
	h.reminders = reminders
	return h
}

// CloseEvent is POST /v1/events/{id}/feedback/close: the panel closed the
// Event's own feedback form (its "Form açık/kapalı" switch, in Forms), and
// tells core so. The reminders still to go of the form's invites are
// cancelled for good: opening the form again does not bring them back.
// Whoever may edit the Event may; 200 {"cancelled": n}.
func (h *FeedbackHandler) CloseEvent(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return eventError(c, err)
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	ev, err := h.events.Get(c.Context(), &p, id)
	if err != nil {
		return eventError(c, err)
	}
	return h.close(c, p, ev, feedback.Target{EventID: ev.ID})
}

// CloseSession is POST /v1/sessions/{id}/feedback/close: the same for the
// Session's form.
func (h *FeedbackHandler) CloseSession(c fiber.Ctx) error {
	p, ev, t, done, err := h.sessionTarget(c)
	if done {
		return err
	}
	return h.close(c, p, ev, t)
}

func (h *FeedbackHandler) close(c fiber.Ctx, p authz.Principal, ev event.Event, t feedback.Target) error {
	if !h.authz.Allow(p, authz.Resource{Type: authz.TypeEvent, OwnerTeam: ev.OwnerTeam}, authz.Update) {
		return eventError(c, event.ErrForbidden)
	}
	n, err := h.reminders.CancelReminders(c.Context(), t, time.Now().UTC())
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"cancelled": n})
}

// SendWith gives the handler its "Şimdi gönder".
func (h *FeedbackHandler) SendWith(sender FeedbackSender) *FeedbackHandler {
	h.sender = sender
	return h
}

// SendEventNow is POST /v1/events/{id}/feedback/send: the Event's own form.
func (h *FeedbackHandler) SendEventNow(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return eventError(c, err)
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	ev, err := h.events.Get(c.Context(), &p, id)
	if err != nil {
		return eventError(c, err)
	}
	return h.sendNow(c, p, ev, feedback.Target{EventID: ev.ID})
}

// SendSessionNow is POST /v1/sessions/{id}/feedback/send: the Session's
// form.
func (h *FeedbackHandler) SendSessionNow(c fiber.Ctx) error {
	p, ev, t, done, err := h.sessionTarget(c)
	if done {
		return err
	}
	return h.sendNow(c, p, ev, t)
}

// sessionTarget reads the Session of the path's id, its Event and the
// target. When it fails, done is true and err is what writing the answer
// to c returned.
func (h *FeedbackHandler) sessionTarget(c fiber.Ctx) (p authz.Principal, ev event.Event, t feedback.Target, done bool, err error) {
	p, err = caller(c)
	if err != nil {
		return p, ev, t, true, eventError(c, err)
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return p, ev, t, true, problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	sess, err := h.events.GetSession(c.Context(), &p, id)
	if err != nil {
		return p, ev, t, true, eventError(c, err)
	}
	day, err := h.events.GetDay(c.Context(), &p, sess.EventDayID)
	if err != nil {
		return p, ev, t, true, eventError(c, err)
	}
	ev, err = h.events.Get(c.Context(), &p, day.EventID)
	if err != nil {
		return p, ev, t, true, eventError(c, err)
	}
	return p, ev, feedback.Target{EventID: ev.ID, SessionID: &sess.ID}, false, nil
}

// sendNow sends the target's form once whoever may edit the Event asks:
// 202 with the number of Tickets queued, 200 with sent false when the target
// was sent already (by the schedule or by hand), 409 when it has no form
// (feedback_form_missing) or may not be sent (feedback_not_sendable: a
// draft, a cancelled Event or Session).
func (h *FeedbackHandler) sendNow(c fiber.Ctx, p authz.Principal, ev event.Event, t feedback.Target) error {
	if !h.authz.Allow(p, authz.Resource{Type: authz.TypeEvent, OwnerTeam: ev.OwnerTeam}, authz.Update) {
		return eventError(c, event.ErrForbidden)
	}
	sent, recipients, err := h.sender.SendNow(c.Context(), t, time.Now().UTC())
	switch {
	case errors.Is(err, feedback.ErrNoForm):
		return problemCode(c, fiber.StatusConflict, "Conflict", "feedback_form_missing")
	case errors.Is(err, feedback.ErrNotSendable):
		return problemCode(c, fiber.StatusConflict, "Conflict", "feedback_not_sendable")
	case err != nil:
		return err
	case !sent:
		return c.JSON(fiber.Map{"sent": false})
	}
	return c.Status(fiber.StatusAccepted).JSON(fiber.Map{"sent": true, "recipients": recipients})
}

func NewFeedbackHandler(events event.Service, az authz.Authorizer, counts FeedbackCounter) *FeedbackHandler {
	return &FeedbackHandler{events: events, authz: az, counts: counts}
}

// Counts is GET /v1/events/{id}/feedback: how many invites the Event's form
// and each Session's form went out in, and how many were answered. Who may
// read the Event in full may (its editors, DK): the counts name nobody, but
// they are the organizers' figures.
func (h *FeedbackHandler) Counts(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return eventError(c, err)
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	ev, err := h.events.Get(c.Context(), &p, id)
	if err != nil {
		return eventError(c, err)
	}
	if !h.authz.Allow(p, authz.Resource{Type: authz.TypeEvent, OwnerTeam: ev.OwnerTeam}, authz.ReadFull) {
		return eventError(c, event.ErrForbidden)
	}
	counts, err := h.counts.Counts(c.Context(), ev.ID)
	if err != nil {
		return err
	}
	return c.JSON(counts)
}
