package handlers

import (
	"encoding/json"
	"log"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/limiter"
	"github.com/gofiber/fiber/v3/middleware/requestid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/clientip"
	"github.com/skylab-kulubu/core-backend/internal/middlewares"
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
	// guestAnonymousPublic is a request without a usable token that is not
	// internal: it came through the public ingress (it carries a forwarding
	// header, middlewares.CameThroughIngress), or its address is not
	// internal (clientip.InternalFromCtx). GuestApply.HideFromEdge answers
	// it 404.
	guestAnonymousPublic guestCaller = iota
	// guestAnonymousInternal is a request without a usable token and without
	// a forwarding header from a private address: the old forms hop, which
	// called core on the internal network without a token. No caller is
	// known today (docs/guest-apply.md).
	guestAnonymousInternal
	// guestPerson is a person's token (the Event hub). Only an operator of
	// the Event is answered; anybody else gets 403 (ticket.Service).
	guestPerson
	// guestService is a service account's token. Only the forms product's
	// with ticket:forms is answered; any other gets 403 (ticket.Service).
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

// GuestApplyLimits are the budgets of Guest apply: each valid token's
// subject (person and service). A request without a token is answered 404
// from the edge before any budget (HideFromEdge); the internal hop is not
// limited, as before.
type GuestApplyLimits struct {
	// PerSubject requests per PerSubjectWindow for each valid token's
	// subject: wide enough for an operator adding guests by hand, narrow
	// enough that one token cannot write guests without end.
	PerSubject       int
	PerSubjectWindow time.Duration
}

// DefaultGuestApplyLimits are the production budgets (docs/guest-apply.md).
func DefaultGuestApplyLimits() GuestApplyLimits {
	return GuestApplyLimits{
		PerSubject:       60,
		PerSubjectWindow: 10 * time.Minute,
	}
}

// GuestApply holds what the Guest apply route does around the handler:
// classify the caller, hide the route from token-less internet callers,
// limit each token, count and log every answer.
type GuestApply struct {
	proxies clientip.Ranges
	metrics *GuestApplyMetrics
	limits  GuestApplyLimits
	logger  *log.Logger
}

// NewGuestApply classifies callers with proxies. A nil metrics counts into
// nothing; a nil logger writes no line.
func NewGuestApply(proxies clientip.Ranges, metrics *GuestApplyMetrics, limits GuestApplyLimits, logger *log.Logger) *GuestApply {
	defaults := DefaultGuestApplyLimits()
	if limits.PerSubject <= 0 || limits.PerSubjectWindow <= 0 {
		limits.PerSubject, limits.PerSubjectWindow = defaults.PerSubject, defaults.PerSubjectWindow
	}
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
	} else if !middlewares.CameThroughIngress(c) && clientip.InternalFromCtx(c, g.proxies) {
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

// HideFromEdge answers a bare 404 to a request without a usable token that
// is not internal (anonymous_public), before the body is read: the route is
// not there for the internet (docs/guest-apply.md). The same answer as the
// internal routes' guard (middlewares.InternalRouteGuard). It runs after the
// optional bearer, so the panel's call with an operator's token, which comes
// through the edge, goes on.
func (g *GuestApply) HideFromEdge(c fiber.Ctx) error {
	if g.caller(c) == guestAnonymousPublic {
		c.Set(fiber.HeaderCacheControl, "no-store")
		return c.SendStatus(fiber.StatusNotFound)
	}
	return c.Next()
}

// Limits are the route's budgets: each token subject (person and service).
func (g *GuestApply) Limits() []fiber.Handler {
	perSubject := limiter.New(limiter.Config{
		Max:        g.limits.PerSubject,
		Expiration: g.limits.PerSubjectWindow,
		Next: func(c fiber.Ctx) bool {
			class := g.caller(c)
			return class != guestPerson && class != guestService
		},
		KeyGenerator: func(c fiber.Ctx) string {
			ident, _ := c.Locals(authn.LocalsIdentity).(authn.Identity)
			return ident.ID.String()
		},
		LimitReached: guestApplyRateLimited,
	})
	return []fiber.Handler{perSubject}
}

// guestApplyRateLimited answers 429.
func guestApplyRateLimited(c fiber.Ctx) error {
	seconds, _ := strconv.Atoi(string(c.Response().Header.Peek(fiber.HeaderRetryAfter)))
	return problemWithFields(c, fiber.StatusTooManyRequests, "Too Many Requests",
		"Too many guest applications; retry after the given seconds.", "guest_apply_rate_limited",
		fiber.Map{"retryAfterSeconds": seconds})
}
