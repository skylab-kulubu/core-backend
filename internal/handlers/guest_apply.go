package handlers

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/netip"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/limiter"
	"github.com/gofiber/fiber/v3/middleware/requestid"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/clientip"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

// localsGuestApplyResult carries the ticket.GuestApplyResult of an answered
// Guest apply from the handler to GuestApply.Observe.
const localsGuestApplyResult = "guest_apply_result"

// localsGuestApplyCaller caches the request's guestCaller once the identity
// is settled.
const localsGuestApplyCaller = "guest_apply_caller"

// guestCaller is who calls Guest apply (docs/guest-apply.md).
type guestCaller int

const (
	// guestAnonymousPublic is a request without a usable token whose client
	// address is outside the trusted proxy ranges: it came from the
	// internet through the edge proxy.
	guestAnonymousPublic guestCaller = iota
	// guestAnonymousInternal is a request without a usable token from inside
	// the trusted proxy ranges: today the forms hop, which calls core on the
	// internal network without a token.
	guestAnonymousInternal
	// guestPerson is a person's token (the Event hub).
	guestPerson
	// guestService is a service account's token.
	guestService
	guestCallerCount
)

var guestCallerNames = [guestCallerCount]string{"anonymous_public", "anonymous_internal", "person", "service"}

// guestOutcome is how a Guest apply ended.
type guestOutcome int

const (
	guestCreated guestOutcome = iota
	guestExisting
	guestKept
	guestInvalid
	guestNotFound
	guestRefused
	guestRateLimited
	guestFailed
	guestOutcomeCount
)

var guestOutcomeNames = [guestOutcomeCount]string{
	"created", "existing", "kept", "invalid", "not_found", "refused", "rate_limited", "failed",
}

// GuestApplyMetrics counts Guest apply requests by caller class and outcome.
// Every label value is fixed; nobody and no Event is named.
type GuestApplyMetrics struct {
	counts [guestCallerCount][guestOutcomeCount]atomic.Uint64
}

func NewGuestApplyMetrics() *GuestApplyMetrics { return &GuestApplyMetrics{} }

func (m *GuestApplyMetrics) record(caller guestCaller, outcome guestOutcome) {
	if m == nil {
		return
	}
	m.counts[caller][outcome].Add(1)
}

// Prometheus renders, in the text exposition format, one counter per caller
// class and outcome, and one total per caller class: the class totals are
// what the switch to enforce waits on (docs/guest-apply.md).
func (m *GuestApplyMetrics) Prometheus() string {
	if m == nil {
		return ""
	}
	var out strings.Builder
	var totals [guestCallerCount]uint64
	out.WriteString("# TYPE skylab_guest_apply_requests_total counter\n")
	for caller := range guestCallerCount {
		for outcome := range guestOutcomeCount {
			value := m.counts[caller][outcome].Load()
			totals[caller] += value
			out.WriteString(`skylab_guest_apply_requests_total{caller="` + guestCallerNames[caller] +
				`",outcome="` + guestOutcomeNames[outcome] + `"} ` + strconv.FormatUint(value, 10) + "\n")
		}
	}
	for caller := range guestCallerCount {
		name := "skylab_guest_apply_" + guestCallerNames[caller] + "_total"
		out.WriteString("# TYPE " + name + " counter\n")
		out.WriteString(name + " " + strconv.FormatUint(totals[caller], 10) + "\n")
	}
	return out.String()
}

// GuestApplyLimits are the budgets of anonymous_public Guest apply requests.
// The other classes are not limited here: the forms hop is the one caller
// every guest's form answer goes through, and a token holder is limited by
// what the token allows.
type GuestApplyLimits struct {
	// PerClient requests per PerClientWindow for each client address.
	PerClient       int
	PerClientWindow time.Duration
	// PerGuest requests per PerGuestWindow for each Event and e-mail, from
	// any address.
	PerGuest       int
	PerGuestWindow time.Duration
}

// DefaultGuestApplyLimits are the production budgets (docs/guest-apply.md).
func DefaultGuestApplyLimits() GuestApplyLimits {
	return GuestApplyLimits{
		PerClient:       20,
		PerClientWindow: 10 * time.Minute,
		PerGuest:        5,
		PerGuestWindow:  time.Hour,
	}
}

// GuestApply holds what the Guest apply route does around the handler:
// classify the caller, limit anonymous internet callers, count and log every
// answer.
type GuestApply struct {
	proxies clientip.Ranges
	metrics *GuestApplyMetrics
	limits  GuestApplyLimits
	logger  *log.Logger
}

// NewGuestApply classifies callers with proxies. A nil metrics counts into
// nothing; a nil logger writes no line.
func NewGuestApply(proxies clientip.Ranges, metrics *GuestApplyMetrics, limits GuestApplyLimits, logger *log.Logger) *GuestApply {
	return &GuestApply{proxies: proxies, metrics: metrics, limits: limits, logger: logger}
}

// caller classifies the request. It reads the identity, so it is only asked
// once the route's bearer middleware has run.
func (g *GuestApply) caller(c fiber.Ctx) guestCaller {
	if cached, ok := c.Locals(localsGuestApplyCaller).(guestCaller); ok {
		return cached
	}
	class := guestAnonymousPublic
	if ident, ok := c.Locals(authn.LocalsIdentity).(authn.Identity); ok {
		class = guestPerson
		if ident.ServiceAccount || ident.Product != "" {
			class = guestService
		}
	} else if addr, err := netip.ParseAddr(clientip.FromCtx(c, g.proxies)); err == nil && g.proxies.Contains(addr) {
		class = guestAnonymousInternal
	}
	c.Locals(localsGuestApplyCaller, class)
	return class
}

// Observe runs first on the route: it answers whatever the rest of the route
// returned, then counts the answer and writes one log line. The line carries
// the correlation id, the caller class and the outcome; never an e-mail, a
// name, a phone number, an address or the Event.
func (g *GuestApply) Observe(c fiber.Ctx) error {
	if err := c.Next(); err != nil {
		if handlerErr := c.App().ErrorHandler(c, err); handlerErr != nil {
			_ = c.SendStatus(fiber.StatusInternalServerError)
		}
	}
	caller := g.caller(c)
	status := c.Response().StatusCode()
	outcome := guestOutcomeOf(c, status)
	g.metrics.record(caller, outcome)
	if g.logger != nil {
		payload, err := json.Marshal(struct {
			Event         string `json:"event"`
			CorrelationID string `json:"correlation_id"`
			Caller        string `json:"caller"`
			Outcome       string `json:"outcome"`
			Status        int    `json:"status"`
		}{"guest_apply", requestid.FromContext(c), guestCallerNames[caller], guestOutcomeNames[outcome], status})
		if err == nil {
			g.logger.Print(string(payload))
		}
	}
	return nil
}

func guestOutcomeOf(c fiber.Ctx, status int) guestOutcome {
	switch {
	case status == fiber.StatusCreated:
		switch c.Locals(localsGuestApplyResult) {
		case ticket.GuestCreated:
			return guestCreated
		case ticket.GuestKept:
			return guestKept
		default:
			return guestExisting
		}
	case status == fiber.StatusTooManyRequests:
		return guestRateLimited
	case status == fiber.StatusUnauthorized || status == fiber.StatusForbidden:
		return guestRefused
	case status == fiber.StatusNotFound:
		return guestNotFound
	case status >= fiber.StatusBadRequest && status < fiber.StatusInternalServerError:
		return guestInvalid
	default:
		return guestFailed
	}
}

// Limits are the route's two budgets for anonymous_public requests, in
// order: each client address, then each Event and e-mail. A request the first
// refuses does not count against the second.
func (g *GuestApply) Limits() []fiber.Handler {
	perClient := limiter.New(limiter.Config{
		Max:        g.limits.PerClient,
		Expiration: g.limits.PerClientWindow,
		Next:       func(c fiber.Ctx) bool { return g.caller(c) != guestAnonymousPublic },
		KeyGenerator: func(c fiber.Ctx) string {
			return clientip.FromCtx(c, g.proxies)
		},
		LimitReached: guestApplyRateLimited,
	})
	perGuest := limiter.New(limiter.Config{
		Max:        g.limits.PerGuest,
		Expiration: g.limits.PerGuestWindow,
		Next: func(c fiber.Ctx) bool {
			if g.caller(c) != guestAnonymousPublic {
				return true
			}
			key := guestKey(c)
			c.Locals(localsGuestKey, key)
			// A body without an e-mail is refused by the handler.
			return key == ""
		},
		KeyGenerator: func(c fiber.Ctx) string {
			key, _ := c.Locals(localsGuestKey).(string)
			return key
		},
		// This budget's X-RateLimit-* headers would tell a caller how many
		// requests others sent for the e-mail lately.
		DisableHeaders: true,
		LimitReached: func(c fiber.Ctx) error {
			c.Set(fiber.HeaderRetryAfter, strconv.FormatInt(int64(g.limits.PerGuestWindow/time.Second), 10))
			return guestApplyRateLimited(c)
		},
	})
	return []fiber.Handler{perClient, perGuest}
}

const localsGuestKey = "guest_apply_key"

// guestKey names the Event and the e-mail of the request without keeping the
// e-mail: a digest of both, or "" when either is missing.
func guestKey(c fiber.Ctx) string {
	eventID, err := uuid.Parse(c.Params("eventId"))
	if err != nil {
		return ""
	}
	var body guestBody
	if err := c.Bind().Body(&body); err != nil {
		return ""
	}
	email := strings.ToLower(strings.TrimSpace(body.Email))
	if email == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(eventID.String() + "\x00" + email))
	return hex.EncodeToString(sum[:])
}

// guestApplyRateLimited answers 429. The answer is the same for both budgets.
func guestApplyRateLimited(c fiber.Ctx) error {
	seconds, _ := strconv.Atoi(string(c.Response().Header.Peek(fiber.HeaderRetryAfter)))
	return problemWithFields(c, fiber.StatusTooManyRequests, "Too Many Requests",
		"Too many guest applications; retry after the given seconds.", "guest_apply_rate_limited",
		fiber.Map{"retryAfterSeconds": seconds})
}
