package middlewares

import (
	"context"
	"encoding/json"
	"errors"
	"log"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/requestid"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/identity"
)

// GroupResolver answers the Group paths of a person whose token carries the
// Group overage marker, and whether the answer was remembered rather than
// read now (identity.OverageGroups).
type GroupResolver interface {
	Paths(ctx context.Context, userID uuid.UUID) ([]string, bool, error)
}

// GroupOverage completes the identity of a token that carries the Group
// overage marker (ADR-0059) with the person's Groups read from Keycloak,
// before any handler decides anything from them. A token with its groups
// claim passes untouched.
//
// When the Groups cannot be read the request is refused with 503 (and
// Retry-After), never decided as a person without Groups or with someone
// else's; with no resolver every marked token is refused. A subject
// Keycloak does not know is refused with 401, like a blocked account.
func GroupOverage(groups GroupResolver) fiber.Handler {
	return groupOverage(groups, log.Default())
}

func groupOverage(groups GroupResolver, logger *log.Logger) fiber.Handler {
	return func(c fiber.Ctx) error {
		ident, ok := c.Locals(authn.LocalsIdentity).(authn.Identity)
		if !ok || !ident.GroupOverage {
			return c.Next()
		}
		var (
			paths  []string
			cached bool
			err    = identity.ErrGroupsUnavailable
		)
		if groups != nil {
			paths, cached, err = groups.Paths(c.Context(), ident.ID)
		}
		switch {
		case errors.Is(err, identity.ErrNotFound):
			logGroupOverage(logger, c, "unknown_subject", 0, nil)
			c.Set(fiber.HeaderWWWAuthenticate, `Bearer error="invalid_token"`)
			return fiber.ErrUnauthorized
		case err != nil:
			logGroupOverage(logger, c, "unavailable", 0, err)
			c.Set(fiber.HeaderCacheControl, "no-store")
			c.Set(fiber.HeaderRetryAfter, "1")
			return fiber.ErrServiceUnavailable
		}
		if !cached {
			logGroupOverage(logger, c, "fetched", len(paths), nil)
		}
		if paths == nil {
			// Keycloak's answer: no Groups. Known, not unknown.
			paths = []string{}
		}
		ident.Groups = paths
		c.Locals(authn.LocalsIdentity, ident)
		return c.Next()
	}
}

// logGroupOverage writes one JSON line per read of a person's Groups: the
// request's correlation id, the outcome and how many paths came back. The
// person, their paths and the token never appear; err names nobody
// (identity.Keycloak.GroupsForUser).
func logGroupOverage(logger *log.Logger, c fiber.Ctx, outcome string, paths int, err error) {
	if logger == nil {
		return
	}
	event := struct {
		Event         string `json:"event"`
		CorrelationID string `json:"correlation_id"`
		Outcome       string `json:"outcome"`
		Paths         int    `json:"paths"`
		Error         string `json:"error,omitempty"`
	}{
		Event:         "group_overage",
		CorrelationID: requestid.FromContext(c),
		Outcome:       outcome,
		Paths:         paths,
	}
	if err != nil {
		event.Error = err.Error()
	}
	if payload, marshalErr := json.Marshal(event); marshalErr == nil {
		logger.Print(string(payload))
	}
}
