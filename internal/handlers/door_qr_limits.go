package handlers

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/limiter"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/clientip"
)

const (
	// GuestFailuresPerIPEnv sets DoorQRLimits.GuestFailuresPerClient.
	GuestFailuresPerIPEnv = "GUEST_CHECKIN_FAILURES_PER_IP"
	// GuestFailuresPerEmailEnv sets DoorQRLimits.GuestFailuresPerEmail.
	GuestFailuresPerEmailEnv = "GUEST_CHECKIN_FAILURES_PER_EMAIL"

	// DefaultGuestFailuresPerClient is sized for a hall: a venue's Wi-Fi can
	// put a few hundred guests behind one address, each of whom may mistype
	// an e-mail or tap twice.
	DefaultGuestFailuresPerClient = 300
	// DefaultGuestFailuresPerEmail is one guest's budget: what one address
	// used to have for everyone.
	DefaultGuestFailuresPerEmail = 10
	maxGuestFailures             = 100000
)

// DoorQRLimits are the budgets of the door QR routes
// (docs/guest-self-check-in.md).
type DoorQRLimits struct {
	// GuestFailuresPerClient is how many failed guest check-ins (anything
	// answered 400 or above, already-checked-in included) one client address
	// may make per GuestWindow, whatever e-mails they carry: the ceiling on
	// trying e-mails from one address. Successful check-ins do not count.
	GuestFailuresPerClient int
	// GuestFailuresPerEmail is the same for one e-mail (or ticket key) from
	// one client address: a guest who keeps tapping spends their own budget,
	// not the hall's.
	GuestFailuresPerEmail int
	GuestWindow           time.Duration
	// MintsPerSubject is how many door QRs one person may mint per
	// MintWindow: a screen asks for one every refreshAfterSeconds (15 s).
	MintsPerSubject int
	MintWindow      time.Duration
}

func DefaultDoorQRLimits() DoorQRLimits {
	return DoorQRLimits{
		GuestFailuresPerClient: DefaultGuestFailuresPerClient,
		GuestFailuresPerEmail:  DefaultGuestFailuresPerEmail,
		GuestWindow:            time.Minute,
		MintsPerSubject:        30, MintWindow: time.Minute,
	}
}

// DoorQRLimitsFromEnv reads GUEST_CHECKIN_FAILURES_PER_IP and
// GUEST_CHECKIN_FAILURES_PER_EMAIL over the defaults. A value that is not a
// whole number from 1 to 100000 is an error, so a typo cannot quietly turn a
// budget off.
func DoorQRLimitsFromEnv(getenv func(string) string) (DoorQRLimits, error) {
	l := DefaultDoorQRLimits()
	for _, setting := range []struct {
		env  string
		dest *int
	}{{GuestFailuresPerIPEnv, &l.GuestFailuresPerClient}, {GuestFailuresPerEmailEnv, &l.GuestFailuresPerEmail}} {
		raw := strings.TrimSpace(getenv(setting.env))
		if raw == "" {
			continue
		}
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxGuestFailures {
			return DoorQRLimits{}, fmt.Errorf("%s: %q is not a whole number from 1 to %d", setting.env, raw, maxGuestFailures)
		}
		*setting.dest = n
	}
	return l, nil
}

// GuestCheckInEmailLimit budgets Guest check-in's failures per client address
// and e-mail (or ticket key): one guest's budget. It runs before
// GuestCheckInLimit, so a request it refuses does not count against the
// address, and one guest who keeps tapping cannot use up everyone's budget.
func (l DoorQRLimits) GuestCheckInEmailLimit(proxies clientip.Ranges) fiber.Handler {
	return limiter.New(limiter.Config{
		Max:                    l.GuestFailuresPerEmail,
		Expiration:             l.GuestWindow,
		SkipSuccessfulRequests: true,
		KeyGenerator: func(c fiber.Ctx) string {
			return clientip.FromCtx(c, proxies) + "|" + guestCheckInSubject(c.Body())
		},
		LimitReached: doorQRRateLimited("guest_check_in_rate_limited"),
	})
}

// GuestCheckInLimit budgets Guest check-in's failures per client address:
// everyone behind it together, so trying many e-mails from one address stays
// bounded.
func (l DoorQRLimits) GuestCheckInLimit(proxies clientip.Ranges) fiber.Handler {
	return limiter.New(limiter.Config{
		Max:                    l.GuestFailuresPerClient,
		Expiration:             l.GuestWindow,
		SkipSuccessfulRequests: true,
		KeyGenerator: func(c fiber.Ctx) string {
			return clientip.FromCtx(c, proxies)
		},
		LimitReached: doorQRRateLimited("guest_check_in_rate_limited"),
	})
}

// guestCheckInSubject names whom a guest check-in is for, as the handler
// picks it: the ticket key when one is sent, else the e-mail, case and
// spaces aside. It is hashed so the limiter's memory holds no addresses and
// a long value costs no more than a short one. A body that does not parse
// shares one subject: the handler answers it 400.
func guestCheckInSubject(raw []byte) string {
	var body struct {
		Email     string `json:"email"`
		TicketKey string `json:"ticketKey"`
	}
	_ = json.Unmarshal(raw, &body)
	subject := "e\x00" + strings.ToLower(strings.TrimSpace(body.Email))
	if key := strings.TrimSpace(body.TicketKey); key != "" {
		subject = "k\x00" + key
	}
	sum := sha256.Sum256([]byte(subject))
	return hex.EncodeToString(sum[:12])
}

// MintLimit budgets door QR minting by the caller's token subject. Requests
// without an identity pass through to the handler, which answers 401.
func (l DoorQRLimits) MintLimit() fiber.Handler {
	return limiter.New(limiter.Config{
		Max:        l.MintsPerSubject,
		Expiration: l.MintWindow,
		Next: func(c fiber.Ctx) bool {
			_, ok := c.Locals(authn.LocalsIdentity).(authn.Identity)
			return !ok
		},
		KeyGenerator: func(c fiber.Ctx) string {
			ident, _ := c.Locals(authn.LocalsIdentity).(authn.Identity)
			return ident.ID.String()
		},
		LimitReached: doorQRRateLimited("door_qr_rate_limited"),
	})
}

func doorQRRateLimited(code string) fiber.Handler {
	return func(c fiber.Ctx) error {
		seconds, _ := strconv.Atoi(string(c.Response().Header.Peek(fiber.HeaderRetryAfter)))
		return problemWithFields(c, fiber.StatusTooManyRequests, "Too Many Requests",
			"Too many requests; retry after the given seconds.", code, fiber.Map{"retryAfterSeconds": seconds})
	}
}
