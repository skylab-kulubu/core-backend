package handlers

import (
	"context"
	"errors"

	"github.com/gofiber/fiber/v3"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/githubactivity"
)

// GithubActivitySource is the club's GitHub activity (githubactivity.Service).
type GithubActivitySource interface {
	Get(ctx context.Context, p authz.Principal) (githubactivity.Activity, error)
}

// GithubActivityHandler serves the admin dashboard's GitHub activity
// (docs/github-activity.md).
type GithubActivityHandler struct {
	source GithubActivitySource
}

func NewGithubActivityHandler(source GithubActivitySource) *GithubActivityHandler {
	return &GithubActivityHandler{source: source}
}

// Get answers the activity to a privileged person: 401 without a bearer, 403
// for anyone else, 503 while it cannot be read (the settings are wrong, or
// GitHub was never reached). A last good answer GitHub could not refresh comes
// with "stale": true.
func (h *GithubActivityHandler) Get(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return problem(c, fiber.StatusUnauthorized, "Unauthorized")
	}
	activity, err := h.source.Get(c.Context(), p)
	switch {
	case errors.Is(err, githubactivity.ErrForbidden):
		return problem(c, fiber.StatusForbidden, "Forbidden")
	case errors.Is(err, githubactivity.ErrUnavailable):
		c.Set(fiber.HeaderCacheControl, "no-store")
		c.Set(fiber.HeaderRetryAfter, "60")
		return problemDetailCode(c, fiber.StatusServiceUnavailable, "Service Unavailable",
			"The GitHub activity cannot be read right now.", "github_activity_unavailable")
	case err != nil:
		return err
	}
	// The same for every privileged person and at most ten minutes old.
	c.Set(fiber.HeaderCacheControl, "private, max-age=60")
	return c.JSON(activity)
}
