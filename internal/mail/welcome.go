package mail

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// WelcomeOutcome is what became of one Welcome mail. The user sync
// (docs/user-sync.md) marks the person welcomed before it sends and keeps the
// mark unless the outcome says no mail task can have been written.
type WelcomeOutcome string

const (
	// WelcomeSent: SkyMail took the mail task (2xx).
	WelcomeSent WelcomeOutcome = "sent"
	// WelcomeRefused: SkyMail answered and wrote no mail task (not 2xx).
	WelcomeRefused WelcomeOutcome = "refused"
	// WelcomeNotSent: nothing left core: no template is configured, no token
	// came, or SkyMail could not be connected to.
	WelcomeNotSent WelcomeOutcome = "not_sent"
	// WelcomeUnknown: the request went out and no answer came back, so
	// SkyMail may have taken it.
	WelcomeUnknown WelcomeOutcome = "unknown"
)

// WelcomeOutcomes lists every outcome, in the order metrics print them.
var WelcomeOutcomes = []WelcomeOutcome{WelcomeSent, WelcomeRefused, WelcomeNotSent, WelcomeUnknown}

// Diagnostic reasons only the Welcome mail logs (the other kinds stay silent
// about them, as they always have).
const (
	reasonTokenUnavailable = "token_unavailable"
	reasonUnreachable      = "unreachable"
	reasonNoAnswer         = "no_answer"
)

// WelcomeConfigured reports whether the Welcome mail can be addressed (a
// template key or id) and sent (a SkyMail address).
func (s *SkyMail) WelcomeConfigured() bool {
	return s != nil && strings.TrimSpace(s.BaseURL) != "" && s.welcomeTemplate().configured()
}

// Welcome posts u's Welcome mail by its template key, falling back to the
// template id when SkyMail does not know the key, and says what became of it.
// Every outcome but sent is warned about in the log (event
// skymail_call_failed, kind welcome) with no recipient, name or template.
func (s *SkyMail) Welcome(ctx context.Context, u user.User) WelcomeOutcome {
	tpl := s.welcomeTemplate()
	if !s.WelcomeConfigured() {
		// Said once at startup (WarnUnconfiguredTemplates).
		return WelcomeNotSent
	}
	token, err := s.Tokens.Token(ctx)
	if err != nil || token == "" {
		warn(s.Logger, eventCallFailed, kindWelcome, 0, reasonTokenUnavailable, "")
		return WelcomeNotSent
	}
	fullName := strings.TrimSpace(u.FirstName + " " + u.LastName)
	vars := map[string]string{
		"FirstName":   u.FirstName,
		"LastName":    u.LastName,
		"Email":       u.Email,
		"Username":    u.Username,
		"SchoolEmail": u.SchoolEmail,
		"SkyNumber":   u.SkyNumber,
		"CreatedAt":   u.CreatedAt.UTC().Format(time.RFC3339),
	}
	attempt := func(t template) (int, WelcomeOutcome) {
		status, reached := s.postReached(ctx, kindWelcome, token, t.task(u.Email, fullName, vars))
		switch {
		case succeeded(status):
			return status, WelcomeSent
		case status != 0:
			return status, WelcomeRefused
		case !reached:
			warn(s.Logger, eventCallFailed, kindWelcome, 0, reasonUnreachable, "")
			return 0, WelcomeNotSent
		default:
			warn(s.Logger, eventCallFailed, kindWelcome, 0, reasonNoAnswer, "")
			return 0, WelcomeUnknown
		}
	}
	if tpl.key != "" {
		status, outcome := attempt(template{key: tpl.key})
		// A key SkyMail does not know is the one refusal the id can still
		// answer, because the key may not be seeded yet.
		if status != http.StatusNotFound || tpl.id == uuid.Nil {
			return outcome
		}
		warn(s.Logger, eventCallFailed, kindWelcome, status, reasonKeyFallback, "")
	}
	_, outcome := attempt(template{id: tpl.id})
	return outcome
}
