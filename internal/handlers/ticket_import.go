package handlers

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/requestid"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

// The attendee import (docs/attendee-import.md):
// POST /v1/events/{eventId}/attendee-imports/preview and
// POST /v1/events/{eventId}/attendee-imports, the list in the body. The
// body is read once and dropped with the answer: no file, no row and no
// log line keeps it.

// maxImportBody is the longest body read: the list's limit and room for the
// JSON around it.
const maxImportBody = ticket.MaxImportBytes + 64<<10

type importBody struct {
	// Text is the list: a CSV file's text (the panel reads the file) or
	// rows pasted from a spreadsheet.
	Text string `json:"text"`
	// SendMail queues each new Ticket's mail; on when left out.
	SendMail *bool `json:"sendMail"`
	// PastCapacity writes every new Ticket active past the capacity, as
	// "Katılımcı ekle" does; off when left out.
	PastCapacity bool `json:"pastCapacity"`
}

// importError answers a refused preview or import.
func importError(c fiber.Ctx, err error) error {
	switch {
	case errors.Is(err, errImportBadID):
		return badID(c, "eventId")
	case errors.Is(err, errImportBadBody):
		return badBody(c)
	case errors.Is(err, ticket.ErrImportTooLarge):
		return problemWithFields(c, fiber.StatusRequestEntityTooLarge, "Payload Too Large",
			"Liste 2 MB'tan büyük.", "import_too_large", fiber.Map{"maxBytes": ticket.MaxImportBytes})
	case errors.Is(err, ticket.ErrImportTooManyRows):
		return problemWithFields(c, fiber.StatusRequestEntityTooLarge, "Payload Too Large",
			"Liste 1000 satırdan uzun.", "import_too_many_rows", fiber.Map{"maxRows": ticket.MaxImportRows})
	case errors.Is(err, ticket.ErrImportEmpty):
		return problemDetailCode(c, fiber.StatusBadRequest, "Bad Request", "Listede satır yok.", "import_empty")
	case errors.Is(err, ticket.ErrImportNoEmail):
		return problemDetailCode(c, fiber.StatusBadRequest, "Bad Request", "Listede e-posta sütunu bulunamadı.", "import_no_email_column")
	case errors.Is(err, ticket.ErrImportAmbiguousEmail):
		var amb *ticket.ImportAmbiguousEmailError
		columns := []string{}
		if errors.As(err, &amb) {
			columns = amb.Columns
		}
		return problemWithFields(c, fiber.StatusBadRequest, "Bad Request",
			"Listede birden çok e-posta sütunu var: "+strings.Join(columns, ", ")+". Yalnız birini bırakın.",
			"import_ambiguous_email_column", fiber.Map{"columns": columns})
	case errors.Is(err, ticket.ErrImportUnreadable):
		return problemDetailCode(c, fiber.StatusBadRequest, "Bad Request", "Liste okunamadı.", "import_unreadable")
	default:
		return resendError(c, err)
	}
}

// errImportBadID and errImportBadBody are a request readImport could not
// read: importError answers them 400 invalid_id and invalid_body.
var (
	errImportBadID   = errors.New("import: event id is not an id")
	errImportBadBody = errors.New("import: body is not JSON")
)

// readImport reads the request: the Event and the body. A bad id is
// errImportBadID, a bad body errImportBadBody, a body past the limit
// ticket.ErrImportTooLarge (importError answers them).
func readImport(c fiber.Ctx) (uuid.UUID, ticket.ImportInput, error) {
	eventID, err := uuid.Parse(c.Params("eventId"))
	if err != nil {
		return uuid.Nil, ticket.ImportInput{}, errImportBadID
	}
	raw := c.Body()
	if len(raw) > maxImportBody {
		return uuid.Nil, ticket.ImportInput{}, ticket.ErrImportTooLarge
	}
	var body importBody
	if err := json.Unmarshal(raw, &body); err != nil {
		return uuid.Nil, ticket.ImportInput{}, errImportBadBody
	}
	in := ticket.ImportInput{Text: body.Text, SendMail: body.SendMail == nil || *body.SendMail, PastCapacity: body.PastCapacity}
	return eventID, in, nil
}

// PreviewImport says what importing the list would do, writing nothing.
func (h *TicketHandler) PreviewImport(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return ticketError(c, err)
	}
	eventID, in, err := readImport(c)
	if err != nil {
		return importError(c, err)
	}
	report, err := h.svc.PreviewImport(c.Context(), p, eventID, in)
	if err != nil {
		return importError(c, err)
	}
	c.Set(fiber.HeaderCacheControl, "no-store")
	return c.JSON(report)
}

// ImportAttendees writes the list's Tickets and answers what each row did.
func (h *TicketHandler) ImportAttendees(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return ticketError(c, err)
	}
	eventID, in, err := readImport(c)
	if err != nil {
		return importError(c, err)
	}
	report, err := h.svc.ImportAttendees(auditContext(c, p), p, eventID, in)
	if err != nil {
		return importError(c, err)
	}
	h.logImport(c, eventID, report)
	c.Set(fiber.HeaderCacheControl, "no-store")
	return c.JSON(report)
}

// logImport writes one line per import: the request, the Event and the
// counts, never a row (the Team audit record keeps who did it).
func (h *TicketHandler) logImport(c fiber.Ctx, eventID uuid.UUID, r ticket.ImportReport) {
	if h.logger == nil {
		return
	}
	if line, err := json.Marshal(struct {
		Event         string               `json:"event"`
		CorrelationID string               `json:"correlation_id"`
		EventID       string               `json:"event_id"`
		Summary       ticket.ImportSummary `json:"summary"`
		Mail          string               `json:"mail"`
	}{"attendees_imported", requestid.FromContext(c), eventID.String(), r.Summary, r.Mail}); err == nil {
		h.logger.Print(string(line))
	}
}
