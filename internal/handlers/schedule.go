package handlers

import (
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/qr"
)

type ScheduleHandler struct {
	svc event.Service
}

func NewScheduleHandler(svc event.Service) *ScheduleHandler {
	return &ScheduleHandler{svc: svc}
}

type dayBody struct {
	EventID   uuid.UUID  `json:"eventId"`
	Name      string     `json:"name"`
	StartDate *time.Time `json:"startDate"`
	EndDate   *time.Time `json:"endDate"`
}

type sessionBody struct {
	EventDayID      uuid.UUID  `json:"eventDayId"`
	Title           string     `json:"title"`
	SpeakerName     string     `json:"speakerName"`
	SpeakerLinkedin string     `json:"speakerLinkedin"`
	Description     string     `json:"description"`
	StartTime       *time.Time `json:"startTime"`
	EndTime         *time.Time `json:"endTime"`
	OrderIndex      int        `json:"orderIndex"`
	SessionType     string     `json:"sessionType"`
	Cancelled       bool       `json:"cancelled"`
}

func (h *ScheduleHandler) ListDays(c fiber.Ctx) error {
	eventID, err := uuid.Parse(c.Params("eventId"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	days, err := h.svc.ListDays(c.Context(), eventID)
	if err != nil {
		return eventError(c, err)
	}
	return c.JSON(days)
}

func (h *ScheduleHandler) GetDay(c fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	day, err := h.svc.GetDay(c.Context(), id)
	if err != nil {
		return eventError(c, err)
	}
	return c.JSON(day)
}

func (h *ScheduleHandler) CreateDay(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return eventError(c, err)
	}
	var body dayBody
	if err := c.Bind().Body(&body); err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	created, err := h.svc.CreateDay(c.Context(), p, event.Day{
		EventID: body.EventID, Name: body.Name, StartDate: body.StartDate, EndDate: body.EndDate,
	})
	if err != nil {
		return eventError(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(created)
}

func (h *ScheduleHandler) UpdateDay(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return eventError(c, err)
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	var body dayBody
	if err := c.Bind().Body(&body); err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	updated, err := h.svc.UpdateDay(c.Context(), p, id, event.Day{
		Name: body.Name, StartDate: body.StartDate, EndDate: body.EndDate,
	})
	if err != nil {
		return eventError(c, err)
	}
	return c.JSON(updated)
}

func (h *ScheduleHandler) DeleteDay(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return eventError(c, err)
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	if err := h.svc.DeleteDay(c.Context(), p, id); err != nil {
		return eventError(c, err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

func (h *ScheduleHandler) ListSessions(c fiber.Ctx) error {
	dayID, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	sessions, err := h.svc.ListSessions(c.Context(), dayID)
	if err != nil {
		return eventError(c, err)
	}
	return c.JSON(sessions)
}

func (h *ScheduleHandler) CurrentSession(c fiber.Ctx) error {
	dayID, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	at := time.Now().UTC()
	if raw := c.Query("at"); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return problem(c, fiber.StatusBadRequest, "Bad Request")
		}
		at = parsed
	}
	cur, err := h.svc.CurrentSession(c.Context(), dayID, at)
	if err != nil {
		return eventError(c, err)
	}
	return c.JSON(cur)
}

func (h *ScheduleHandler) GetSession(c fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	sess, err := h.svc.GetSession(c.Context(), id)
	if err != nil {
		return eventError(c, err)
	}
	return c.JSON(sess)
}

func (h *ScheduleHandler) SessionQR(c fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	if _, err := h.svc.GetSession(c.Context(), id); err != nil {
		return eventError(c, err)
	}
	size := qr.SizeFromQuery(c.Query("size"))
	content := qr.SessionURL(id.String())
	var png []byte
	if qr.LogoFromQuery(c.Query("logo")) {
		png, err = qr.PNGWithLogo(content, size)
	} else {
		png, err = qr.PNG(content, size)
	}
	if err != nil {
		return err
	}
	c.Set(fiber.HeaderContentType, "image/png")
	return c.Send(png)
}

func (h *ScheduleHandler) CreateSession(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return eventError(c, err)
	}
	var body sessionBody
	if err := c.Bind().Body(&body); err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	created, err := h.svc.CreateSession(c.Context(), p, event.Session{
		EventDayID: body.EventDayID, Title: body.Title, SpeakerName: body.SpeakerName,
		SpeakerLinkedin: body.SpeakerLinkedin, Description: body.Description,
		StartTime: body.StartTime, EndTime: body.EndTime, OrderIndex: body.OrderIndex, SessionType: body.SessionType,
		Cancelled: body.Cancelled,
	})
	if err != nil {
		return eventError(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(created)
}

func (h *ScheduleHandler) UpdateSession(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return eventError(c, err)
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	var body sessionBody
	if err := c.Bind().Body(&body); err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	updated, err := h.svc.UpdateSession(c.Context(), p, id, event.Session{
		Title: body.Title, SpeakerName: body.SpeakerName, SpeakerLinkedin: body.SpeakerLinkedin,
		Description: body.Description, StartTime: body.StartTime, EndTime: body.EndTime,
		OrderIndex: body.OrderIndex, SessionType: body.SessionType, Cancelled: body.Cancelled,
	})
	if err != nil {
		return eventError(c, err)
	}
	return c.JSON(updated)
}

func (h *ScheduleHandler) DeleteSession(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return eventError(c, err)
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	if err := h.svc.DeleteSession(c.Context(), p, id); err != nil {
		return eventError(c, err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}
