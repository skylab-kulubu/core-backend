package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/requestid"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/attendeeexport"
	"github.com/skylab-kulubu/core-backend/internal/certificate"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

// The attendee export (docs/attendee-export.md):
// POST /v1/events/{eventId}/exports answers a report of the Event's
// attendees as an XLSX or CSV file, for a reader of its Tickets. Each
// download is a ticket.export row of the Team audit record; the file is
// made in memory and kept nowhere.

// StandingsReader is the Event's attendance by its certificate rule
// (certificate.Service.Standings).
type StandingsReader interface {
	Standings(ctx context.Context, ev event.Event) (certificate.Standings, error)
}

// ExportHandler serves the attendee export.
type ExportHandler struct {
	tickets   ticket.Service
	standings StandingsReader
	logger    *log.Logger
	now       func() time.Time
}

// NewExportHandler serves exports from tickets; standings nil writes no
// certificate counts.
func NewExportHandler(tickets ticket.Service, standings StandingsReader) *ExportHandler {
	return &ExportHandler{tickets: tickets, standings: standings, logger: log.Default(), now: time.Now}
}

type exportBody struct {
	Report    string     `json:"report"`
	Format    string     `json:"format"`
	SessionID *uuid.UUID `json:"sessionId"`
	// TicketIDs, when present, are the rows the panel shows: the file has
	// those alone, in that order.
	TicketIDs *[]uuid.UUID `json:"ticketIds"`
}

// Export is POST /v1/events/:eventId/exports.
func (h *ExportHandler) Export(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return ticketError(c, err)
	}
	eventID, err := uuid.Parse(c.Params("eventId"))
	if err != nil {
		return badID(c, "eventId")
	}
	var body exportBody
	if err := json.Unmarshal(c.Body(), &body); err != nil {
		return badBody(c)
	}
	req := ticket.ExportRequest{Report: body.Report, Format: body.Format}
	if req.Format == "" {
		req.Format = "xlsx"
	}
	if body.SessionID != nil {
		req.SessionID = *body.SessionID
	}
	if body.TicketIDs != nil {
		req.Selected, req.TicketIDs = true, *body.TicketIDs
	}
	data, err := h.tickets.BeginExport(auditContext(c, p), p, eventID, req)
	if err != nil {
		if errors.Is(err, ticket.ErrExportInvalid) {
			return problemDetailCode(c, fiber.StatusBadRequest, "Bad Request", "Bu dışa aktarma yapılamıyor.", "invalid_export")
		}
		return resendError(c, err)
	}
	file, err := h.make(c.Context(), data)
	h.tickets.FinishExport(c.Context(), data, file.Rows, err)
	if err != nil {
		return err
	}
	if h.logger != nil {
		// One line per export: the request, the Event and the file's
		// shape, never a row's content or who asked (the audit row keeps
		// that).
		if line, err := json.Marshal(struct {
			Event         string `json:"event"`
			CorrelationID string `json:"correlation_id"`
			EventID       string `json:"event_id"`
			Report        string `json:"report"`
			Format        string `json:"format"`
			Selected      bool   `json:"selected"`
			Rows          int    `json:"rows"`
		}{"attendees_exported", requestid.FromContext(c), eventID.String(), req.Report, req.Format, req.Selected, file.Rows}); err == nil {
			h.logger.Print(string(line))
		}
	}
	c.Set(fiber.HeaderContentType, file.ContentType)
	c.Set(fiber.HeaderContentDisposition, attendeeexport.Disposition(file.Name))
	c.Set(fiber.HeaderCacheControl, "no-store")
	c.Set(fiber.HeaderXContentTypeOptions, "nosniff")
	return c.Status(fiber.StatusOK).Send(file.Body)
}

func (h *ExportHandler) make(ctx context.Context, data *ticket.ExportData) (attendeeexport.File, error) {
	var standings *certificate.Standings
	if h.standings != nil && attendeeexport.NeedsStandings(data.Request.Report) {
		read, err := h.standings.Standings(ctx, data.Event)
		if err != nil {
			return attendeeexport.File{}, err
		}
		standings = &read
	}
	table, err := attendeeexport.Build(data, standings)
	if err != nil {
		return attendeeexport.File{}, err
	}
	return attendeeexport.Make(data, table, h.now())
}
