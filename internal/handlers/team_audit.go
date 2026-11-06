package handlers

import (
	"encoding/json"
	"errors"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/identity"
)

// TeamAuditRecords answers GET /v1/team-audit-records (docs/team-audit.md,
// ekip-yetki-devri ticket 10): a page of the Team audit record for the
// audit:read role, newest first, and the next page's token ("next", null on
// the last). Every read, a refused one too, is one log line: who and which
// kinds of filter, never their values.
func (h *IdentityHandler) TeamAuditRecords(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return identityError(c, err)
	}
	c.Set(fiber.HeaderCacheControl, "no-store")
	q, kinds := auditQueryFrom(c), auditFilterKinds(c)
	page, err := h.svc.TeamAuditRecords(c.Context(), p, q)
	if err != nil {
		outcome := "error"
		switch {
		case errors.Is(err, identity.ErrForbidden):
			outcome = "forbidden"
		case errors.Is(err, identity.ErrInvalid):
			outcome = "invalid"
		}
		logAuditRead(p.ID, p.Client, kinds, outcome, 0)
		return identityError(c, err)
	}
	logAuditRead(p.ID, p.Client, kinds, "ok", len(page.Items))
	answer := fiber.Map{"items": page.Items, "next": nil}
	if page.Next != "" {
		answer["next"] = page.Next
	}
	return c.JSON(answer)
}

// auditQueryFrom reads the route's query string. action may repeat and may
// hold several actions separated by commas; from and to are RFC 3339. A
// value it cannot read marks the query Malformed: the service answers 400
// after it has decided the caller may read at all.
func auditQueryFrom(c fiber.Ctx) identity.AuditQuery {
	q := identity.AuditQuery{GroupID: c.Query("groupId"), Outcome: c.Query("outcome"), Cursor: c.Query("cursor")}
	bad := false
	switch c.Query("subtree") {
	case "", "false":
	case "true":
		q.Subtree = true
	default:
		bad = true
	}
	person := func(key string) uuid.UUID {
		raw := c.Query(key)
		if raw == "" {
			return uuid.Nil
		}
		id, err := uuid.Parse(raw)
		if err != nil || id == uuid.Nil {
			bad = true
		}
		return id
	}
	q.ActorID, q.UserID = person("actorId"), person("userId")
	for _, raw := range c.Request().URI().QueryArgs().PeekMulti("action") {
		for _, a := range strings.Split(string(raw), ",") {
			if a = strings.TrimSpace(a); a != "" {
				q.Actions = append(q.Actions, a)
			}
		}
	}
	at := func(key string) time.Time {
		raw := c.Query(key)
		if raw == "" {
			return time.Time{}
		}
		t, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			bad = true
		}
		return t
	}
	q.From, q.To = at("from"), at("to")
	if raw := c.Query("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			bad = true
		}
		q.Limit = n
	}
	q.Malformed = bad
	return q
}

// auditFilters are the route's filters, in the order the log names them.
var auditFilters = []string{"groupId", "subtree", "actorId", "userId", "action", "outcome", "from", "to", "cursor", "limit"}

// auditFilterKinds are the filters the query string carries, read or not:
// their names only.
func auditFilterKinds(c fiber.Ctx) []string {
	out := []string{}
	args := c.Request().URI().QueryArgs()
	for _, name := range auditFilters {
		if args.Has(name) {
			out = append(out, name)
		}
	}
	return out
}

// logAuditRead writes the access log line of one read of the record: who,
// the kinds of filter (never their values) and how it ended.
func logAuditRead(actor, client string, filters []string, outcome string, rows int) {
	line, err := json.Marshal(struct {
		Event   string   `json:"event"`
		Actor   string   `json:"actor"`
		Client  string   `json:"client"`
		Filters []string `json:"filters"`
		Outcome string   `json:"outcome"`
		Rows    int      `json:"rows"`
	}{"team_audit_read", actor, client, filters, outcome, rows})
	if err != nil {
		return
	}
	log.Print(string(line))
}
