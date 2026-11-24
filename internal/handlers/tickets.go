package handlers

import (
	"bytes"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/requestid"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/doorqr"
	"github.com/skylab-kulubu/core-backend/internal/qr"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

type TicketHandler struct {
	svc    ticket.Service
	logger *log.Logger
}

func NewTicketHandler(svc ticket.Service) *TicketHandler {
	return &TicketHandler{svc: svc, logger: log.Default()}
}

type guestBody struct {
	FirstName   string `json:"firstName"`
	LastName    string `json:"lastName"`
	Email       string `json:"email"`
	PhoneNumber string `json:"phoneNumber"`
	// JoinWaitlist asks for the waiting list of a full Event
	// (docs/event-capacity.md); without it a full Event is 409 event_full.
	JoinWaitlist bool `json:"joinWaitlist"`
}

// applyBody is Member apply's optional body: joinWaitlist as Guest apply's.
// A client that sends none (sky-app before the waiting list) is refused a
// full Event, never put on its list unasked.
type applyBody struct {
	JoinWaitlist bool `json:"joinWaitlist"`
}

func ticketError(c fiber.Ctx, err error) error {
	switch {
	case errors.Is(err, fiber.ErrUnauthorized):
		return problem(c, fiber.StatusUnauthorized, "Unauthorized")
	case errors.Is(err, ticket.ErrForbidden):
		return problem(c, fiber.StatusForbidden, "Forbidden")
	case errors.Is(err, ticket.ErrNotFound):
		return problem(c, fiber.StatusNotFound, "Not Found")
	case errors.Is(err, ticket.ErrTicketCancelled):
		return problemCode(c, fiber.StatusConflict, "Conflict", "ticket_cancelled")
	case errors.Is(err, ticket.ErrTicketWaitlisted):
		return problemCode(c, fiber.StatusConflict, "Conflict", "ticket_waitlisted")
	case errors.Is(err, ticket.ErrEventFull):
		// The Event is full (docs/event-capacity.md): waitlistEnabled tells
		// the client whether it may offer the waiting list.
		var full *ticket.EventFullError
		enabled := errors.As(err, &full) && full.WaitlistEnabled
		return problemWithFields(c, fiber.StatusConflict, "Conflict", "Conflict", "event_full",
			fiber.Map{"waitlistEnabled": enabled})
	case errors.Is(err, ticket.ErrConflict):
		return problemCode(c, fiber.StatusConflict, "Conflict", eventStatusCode(err))
	case errors.Is(err, ticket.ErrAmbiguous):
		return problem(c, fiber.StatusConflict, "Ambiguous Match")
	case errors.Is(err, ticket.ErrInvalid):
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	case errors.Is(err, ticket.ErrDoorQRRequired):
		return problemCode(c, fiber.StatusForbidden, "Door QR Required", "door_qr_required")
	case errors.Is(err, ticket.ErrDoorQRInvalid):
		return problemCode(c, fiber.StatusForbidden, "Door QR Invalid", "door_qr_invalid")
	case errors.Is(err, ticket.ErrDoorQRExpired):
		return problemCode(c, fiber.StatusForbidden, "Door QR Expired", "door_qr_expired")
	case errors.Is(err, ticket.ErrDoorQRUsedUp):
		return problemCode(c, fiber.StatusForbidden, "Door QR Used Up", "door_qr_used_up")
	case errors.Is(err, ticket.ErrSessionClosed):
		// The door's "session not open"; reason says why when the Event's
		// status is (event_not_published), for a client that knows it.
		var fields fiber.Map
		if reason := eventStatusCode(err); reason != "" {
			fields = fiber.Map{"reason": reason}
		}
		return problemWithFields(c, fiber.StatusForbidden, "Session Not Open", "Session Not Open", "session_closed", fields)
	default:
		return err
	}
}

func (h *TicketHandler) Apply(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return ticketError(c, err)
	}
	eventID, err := uuid.Parse(c.Params("eventId"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	var body applyBody
	if raw := c.Body(); len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			return problem(c, fiber.StatusBadRequest, "Bad Request")
		}
	}
	created, err := h.svc.ApplyJoining(c.Context(), p, eventID, body.JoinWaitlist)
	if err != nil {
		return ticketError(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(created)
}

func (h *TicketHandler) ApplyForOther(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return ticketError(c, err)
	}
	eventID, err := uuid.Parse(c.Params("eventId"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	userID, err := uuid.Parse(c.Params("userId"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	created, err := h.svc.ApplyForOther(c.Context(), p, eventID, userID)
	if err != nil {
		return ticketError(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(created)
}

func (h *TicketHandler) ListAssignableUsers(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return ticketError(c, err)
	}
	eventID, err := uuid.Parse(c.Params("eventId"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	people, err := h.svc.ListAssignableUsers(c.Context(), p, eventID, c.Query("q"))
	if err != nil {
		return ticketError(c, err)
	}
	return c.JSON(people)
}

func (h *TicketHandler) ListDoorEvents(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return ticketError(c, err)
	}
	events, err := h.svc.ListDoorEvents(c.Context(), p)
	if err != nil {
		return ticketError(c, err)
	}
	return c.JSON(events)
}

func (h *TicketHandler) SearchDoorAttendees(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return ticketError(c, err)
	}
	eventID, err := uuid.Parse(c.Params("eventId"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	attendees, err := h.svc.SearchDoorAttendees(c.Context(), p, eventID, c.Query("q"))
	if err != nil {
		return ticketError(c, err)
	}
	return c.JSON(attendees)
}

// guestApplied is Guest apply's answer to a caller who may not see the
// Ticket. It is the same for a new guest and for one already registered, so
// the answer does not tell whether the e-mail had applied.
type guestApplied struct {
	Status string `json:"status"`
}

// ApplyGuest is Guest apply (docs/guest-apply.md). The route takes a token
// but does not require one: an operator of the Event and a product's service
// identity get the Ticket, as before; anybody else gets guestApplied.
func (h *TicketHandler) ApplyGuest(c fiber.Ctx) error {
	eventID, err := uuid.Parse(c.Params("eventId"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	var body guestBody
	if err := c.Bind().Body(&body); err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	applied, err := h.svc.ApplyGuestJoining(c.Context(), optionalCaller(c), eventID, ticket.GuestInfo{
		FirstName: body.FirstName, LastName: body.LastName, Email: body.Email, PhoneNumber: body.PhoneNumber,
	}, body.JoinWaitlist)
	if err != nil {
		return ticketError(c, err)
	}
	c.Locals(localsGuestApplyResult, applied.Result)
	if !applied.Trusted {
		// "waitlisted" on a full Event's waiting list, from the Event, never
		// from a Ticket already there (docs/event-capacity.md).
		status := "applied"
		if applied.Waitlisted {
			status = "waitlisted"
		}
		return c.Status(fiber.StatusCreated).JSON(guestApplied{Status: status})
	}
	return c.Status(fiber.StatusCreated).JSON(applied.Ticket)
}

type formResponseBody struct {
	ResponseID uuid.UUID  `json:"responseId"`
	Status     string     `json:"status"`
	UserID     *uuid.UUID `json:"userId"`
	Guest      *guestBody `json:"guest"`
	// SubmittedAt is when the forms service first took the answer,
	// optional (ticket 12): read by submittedAt, which never refuses the
	// report for it.
	SubmittedAt json.RawMessage `json:"submittedAt"`
	// Invite is the feedback invite's code the answer came with, optional
	// (etkinlik geri bildirim formu, ticket 04): an unknown one, another
	// form's or one longer than 64 characters is ignored. Never logged.
	Invite string `json:"invite"`
}

// RecordFormResponse takes the forms service's report of an answer to one of
// its forms (docs/form-response-tickets.md). The answer is 204 whether or not
// an Event lists the form: the forms service only needs to know it may stop
// sending the report. An answer that writes no Ticket because every Event of
// the form is cancelled (ticket 04), or, for an answer whose submittedAt the
// forms service sent, closed to applications by then (ticket 12), or ended
// (ADR 0069, addendum of 2026-11-22), is 200 with {"outcome":
// "event_cancelled"}, {"outcome": "registration_closed"} or {"outcome":
// "event_ended"}: still a 2xx, which the forms service takes as done. An
// accepted answer to a full Event is 200 {"outcome": "waitlisted"} (on its
// waiting list) or {"outcome": "event_full"} (waiting list off; no Ticket;
// docs/event-capacity.md). A caller who may not
// report is refused before the request is read, so a malformed request from
// them is 403 too.
func (h *TicketHandler) RecordFormResponse(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return ticketError(c, err)
	}
	if !h.svc.CanRecordFormResponse(p) {
		return ticketError(c, ticket.ErrForbidden)
	}
	formID, err := uuid.Parse(c.Params("formId"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	var body formResponseBody
	if err := c.Bind().Body(&body); err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	r := ticket.FormResponse{
		FormID:      formID,
		ResponseID:  body.ResponseID,
		Status:      ticket.FormResponseStatus(body.Status),
		UserID:      body.UserID,
		SubmittedAt: h.submittedAt(formID, body.SubmittedAt),
		Invite:      body.Invite,
	}
	if body.Guest != nil {
		r.Guest = &ticket.GuestInfo{FirstName: body.Guest.FirstName, LastName: body.Guest.LastName, Email: body.Guest.Email}
	}
	result, err := h.svc.RecordFormResponse(c.Context(), p, r)
	h.logFormResponse(c, r, result, err)
	if err != nil {
		return ticketError(c, err)
	}
	switch result.Outcome {
	case ticket.FormResponseEventCancelled, ticket.FormResponseRegistrationClosed, ticket.FormResponseEventEnded,
		ticket.FormResponseWaitlisted, ticket.FormResponseEventFull:
		return c.JSON(fiber.Map{"outcome": result.Outcome})
	default:
		return c.SendStatus(fiber.StatusNoContent)
	}
}

// submittedAt reads a report's submittedAt: an RFC 3339 time with its zone
// (Forms sends UTC, with Z). A value that is not one (no zone, not a
// string) is left out, as if not sent, and the report is not refused for it:
// the answer then earns its Ticket. The warning names the form, never who
// answered.
func (h *TicketHandler) submittedAt(formID uuid.UUID, raw json.RawMessage) *time.Time {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		if at, err := time.Parse(time.RFC3339Nano, text); err == nil {
			return &at
		}
	}
	logger := h.logger
	if logger == nil {
		logger = log.Default()
	}
	payload, err := json.Marshal(struct {
		Event  string `json:"event"`
		Level  string `json:"level"`
		FormID string `json:"form_id"`
		Reason string `json:"reason"`
	}{"form_response_submitted_at_ignored", "warn", formID.String(), "not an RFC 3339 time with a zone"})
	if err == nil {
		logger.Print(string(payload))
	}
	return nil
}

// logFormResponse writes one JSON line per report core reads: the request's
// correlation id, the form, the answer's id in Forms, the reported status,
// how the report ended, how many Tickets it wrote and how many current
// Events list the form (more than one: a form shared before the one-place
// rule, ADR 0069 addendum of 2026-11-22; 0 when that was not read). Who
// answered (the
// userId, a guest's name or e-mail) never appears, nor does err.
func (h *TicketHandler) logFormResponse(c fiber.Ctx, r ticket.FormResponse, result ticket.FormResponseResult, err error) {
	if h.logger == nil {
		return
	}
	status := string(r.Status)
	switch r.Status {
	case ticket.FormResponsePending, ticket.FormResponseAccepted, ticket.FormResponseDeclined:
	default:
		status = "unknown"
	}
	outcome := string(result.Outcome)
	switch {
	case errors.Is(err, ticket.ErrInvalid):
		outcome = "invalid"
	case err != nil:
		outcome = "failed"
	}
	payload, marshalErr := json.Marshal(struct {
		Event          string `json:"event"`
		CorrelationID  string `json:"correlation_id"`
		FormID         string `json:"form_id"`
		ResponseID     string `json:"response_id"`
		Status         string `json:"status"`
		Outcome        string `json:"outcome"`
		TicketsWritten int    `json:"tickets_written"`
		ListingEvents  int    `json:"listing_events"`
	}{"form_response", requestid.FromContext(c), r.FormID.String(), r.ResponseID.String(), status, outcome, result.TicketsWritten, result.ListingEvents})
	if marshalErr == nil {
		h.logger.Print(string(payload))
	}
}

func (h *TicketHandler) Mine(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return ticketError(c, err)
	}
	include, ok := statusList(c.Query("include"))
	if !ok {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	tickets, err := h.svc.MineIncluding(c.Context(), p, include)
	if err != nil {
		return ticketError(c, err)
	}
	return c.JSON(tickets)
}

func (h *TicketHandler) ListByEvent(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return ticketError(c, err)
	}
	eventID, err := uuid.Parse(c.Params("eventId"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	// ?status=active (the default), waitlisted, cancelled or all.
	var statuses []ticket.Status
	switch raw := strings.TrimSpace(c.Query("status")); raw {
	case "", string(ticket.StatusActive):
		statuses = []ticket.Status{ticket.StatusActive}
	case "all":
	default:
		if !ticket.ValidStatus(ticket.Status(raw)) {
			return problem(c, fiber.StatusBadRequest, "Bad Request")
		}
		statuses = []ticket.Status{ticket.Status(raw)}
	}
	tickets, err := h.svc.ListByEventStatus(c.Context(), p, eventID, statuses)
	if err != nil {
		return ticketError(c, err)
	}
	return c.JSON(tickets)
}

func (h *TicketHandler) CheckIn(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return ticketError(c, err)
	}
	ticketID, err := uuid.Parse(c.Params("ticketId"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	sessionID, err := uuid.Parse(c.Params("sessionId"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	ci, err := h.svc.CheckIn(c.Context(), p, ticketID, sessionID)
	if err != nil {
		return ticketError(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(ci)
}

// UndoCheckIn answers DELETE /v1/tickets/:ticketId/sessions/:sessionId/check-in:
// the door desk takes back a wrong check-in. 204 when it is gone, 404 when
// the Ticket had none at the Session; the rest as CheckIn.
func (h *TicketHandler) UndoCheckIn(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return ticketError(c, err)
	}
	ticketID, err := uuid.Parse(c.Params("ticketId"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	sessionID, err := uuid.Parse(c.Params("sessionId"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	if err := h.svc.UndoCheckIn(c.Context(), p, ticketID, sessionID); err != nil {
		return ticketError(c, err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

// UndoCheckInByID answers DELETE /v1/sessions/:sessionId/check-ins/:checkInId:
// UndoCheckIn by the check-in's own id, the one the door activity and the
// resolve and scan answers carry. 404 for a check-in of another Session.
func (h *TicketHandler) UndoCheckInByID(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return ticketError(c, err)
	}
	sessionID, err := uuid.Parse(c.Params("sessionId"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	checkInID, err := uuid.Parse(c.Params("checkInId"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	if err := h.svc.UndoCheckInByID(c.Context(), p, sessionID, checkInID); err != nil {
		return ticketError(c, err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

type guestCheckInBody struct {
	Email     string `json:"email"`
	DoorToken string `json:"doorToken"`
}

// selfCheckInBody is check-in/me's optional body. sky-app's versions that
// read only the static Session QR send none.
type selfCheckInBody struct {
	DoorToken string `json:"doorToken"`
}

type resolveCheckInBody struct {
	PersonID string `json:"personId"`
	Query    string `json:"query"`
}

func (h *TicketHandler) CheckInMe(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return ticketError(c, err)
	}
	sessionID, err := uuid.Parse(c.Params("sessionId"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	var body selfCheckInBody
	if raw := bytes.TrimSpace(c.Body()); len(raw) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			return problem(c, fiber.StatusBadRequest, "Bad Request")
		}
	}
	ci, err := h.svc.CheckInMe(c.Context(), p, sessionID, body.DoorToken)
	if err != nil {
		return ticketError(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(ci)
}

func (h *TicketHandler) CheckInGuest(c fiber.Ctx) error {
	sessionID, err := uuid.Parse(c.Params("sessionId"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	var body guestCheckInBody
	if err := c.Bind().Body(&body); err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	ci, err := h.svc.CheckInGuest(c.Context(), sessionID, ticket.GuestCheckIn{Email: body.Email, DoorToken: body.DoorToken})
	if err != nil {
		return ticketError(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(ci)
}

// MintDoorQR answers the door screen: a fresh signed door QR for the Session
// (docs/guest-self-check-in.md). ?use=display mints the attendance QR for a
// projection screen instead, with its own use cap.
func (h *TicketHandler) MintDoorQR(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return ticketError(c, err)
	}
	sessionID, err := uuid.Parse(c.Params("sessionId"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	class, ok := doorqr.ParseClass(c.Query("use"))
	if !ok {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	pass, err := h.svc.MintDoorQR(c.Context(), p, sessionID, class)
	if err != nil {
		return ticketError(c, err)
	}
	c.Set(fiber.HeaderCacheControl, "no-store")
	if !queryFlag(c.Query("svg")) {
		return c.Status(fiber.StatusCreated).JSON(pass)
	}
	// ?svg=1: the QR itself, drawn from pass.URL, for a screen that has no QR
	// library. Square modules, no logo: it is redrawn every few seconds and
	// scanned from further away than a poster.
	svg, err := qr.PlainSVG(pass.URL)
	if err != nil {
		return err
	}
	return c.Status(fiber.StatusCreated).JSON(doorQRWithSVG{DoorQR: pass, SVG: string(svg)})
}

// queryFlag reads a yes/no query parameter: 1, true or yes is yes.
func queryFlag(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

type doorQRWithSVG struct {
	ticket.DoorQR
	SVG string `json:"svg"`
}

func (h *TicketHandler) ResolveAndCheckIn(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return ticketError(c, err)
	}
	sessionID, err := uuid.Parse(c.Params("sessionId"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	var body resolveCheckInBody
	if err := c.Bind().Body(&body); err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	target := ticket.DoorCheckInTarget{Query: body.Query}
	if body.PersonID != "" {
		personID, err := uuid.Parse(body.PersonID)
		if err != nil {
			return problem(c, fiber.StatusBadRequest, "Bad Request")
		}
		target.PersonID = &personID
	}
	created, err := h.svc.ResolveAndCheckIn(c.Context(), p, sessionID, target)
	if err != nil {
		return ticketError(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(created)
}

func (h *TicketHandler) DoorActivity(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return ticketError(c, err)
	}
	sessionID, err := uuid.Parse(c.Params("sessionId"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	activity, err := h.svc.DoorActivity(c.Context(), p, sessionID)
	if err != nil {
		return ticketError(c, err)
	}
	return c.JSON(activity)
}

func (h *TicketHandler) Get(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return ticketError(c, err)
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	got, err := h.svc.Get(c.Context(), p, id)
	if err != nil {
		return ticketError(c, err)
	}
	return c.JSON(got)
}

func (h *TicketHandler) ByUserEvent(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return ticketError(c, err)
	}
	userID, err := uuid.Parse(c.Params("userId"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	eventID, err := uuid.Parse(c.Params("eventId"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	got, err := h.svc.GetByUserEvent(c.Context(), p, userID, eventID)
	if err != nil {
		return ticketError(c, err)
	}
	return c.JSON(got)
}

func (h *TicketHandler) List(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return ticketError(c, err)
	}
	email := c.Query("email")
	var userID *uuid.UUID
	if raw := c.Query("userId"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			return problem(c, fiber.StatusBadRequest, "Bad Request")
		}
		userID = &id
	}
	tickets, err := h.svc.ListQuery(c.Context(), p, email, userID)
	if err != nil {
		return ticketError(c, err)
	}
	return c.JSON(tickets)
}
