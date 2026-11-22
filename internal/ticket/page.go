package ticket

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/skylab-kulubu/core-backend/internal/event"
)

// The ticket page (ADR 0082; docs/ticket-page.md): the club site's page
// that opens a Ticket by its access key, without a sign-in.
const (
	// PageURLEnv is the ticket page's address, with KeyPlaceholder where the
	// access key goes (https://yildizskylab.com/bilet/{key}). Empty, the
	// default: core makes no link, and the mail goes without one.
	PageURLEnv = "TICKET_PAGE_URL"
	// PageGraceEnv is how long after its Event ends a ticket page stays
	// open (a Go duration); then it and its QR and PDF answer 410.
	PageGraceEnv = "TICKET_PAGE_GRACE"
	// KeyPlaceholder is replaced by the access key in PageURLEnv.
	KeyPlaceholder = "{key}"

	DefaultPageGrace = 7 * 24 * time.Hour
	MaxPageGrace     = 90 * 24 * time.Hour
)

// PageConfig is the ticket page's settings.
type PageConfig struct {
	// URL is PageURLEnv; empty makes no link.
	URL string
	// Grace is PageGraceEnv.
	Grace time.Duration
}

// PageConfigFromEnv reads the settings. A value that does not parse stops
// core: a typo must not send links nobody can open.
func PageConfigFromEnv(getenv func(string) string) (PageConfig, error) {
	cfg := PageConfig{Grace: DefaultPageGrace}
	if raw := strings.TrimSpace(getenv(PageURLEnv)); raw != "" {
		u, err := url.Parse(strings.ReplaceAll(raw, KeyPlaceholder, "KEY"))
		if err != nil || u.Scheme != "https" || u.Host == "" || strings.Count(raw, KeyPlaceholder) != 1 || u.RawQuery != "" || u.Fragment != "" {
			return PageConfig{}, fmt.Errorf("%s: want an https URL with %s once and no query", PageURLEnv, KeyPlaceholder)
		}
		cfg.URL = raw
	}
	if raw := strings.TrimSpace(getenv(PageGraceEnv)); raw != "" {
		grace, err := time.ParseDuration(raw)
		if err != nil {
			return PageConfig{}, fmt.Errorf("%s: %w", PageGraceEnv, err)
		}
		if grace < 0 || grace > MaxPageGrace {
			return PageConfig{}, fmt.Errorf("%s: %s is outside 0–%s", PageGraceEnv, grace, MaxPageGrace)
		}
		cfg.Grace = grace
	}
	return cfg, nil
}

// Link is the ticket page of key; "" when there is no page address or no
// key.
func (c PageConfig) Link(key string) string {
	if c.URL == "" || !ValidAccessKey(key) {
		return ""
	}
	return strings.Replace(c.URL, KeyPlaceholder, key, 1)
}

// Closed reports whether ev's ticket pages are closed at now: Grace after
// the Event ended. An Event with no date never ends, so its pages never
// close.
func (c PageConfig) Closed(ev event.Event, now time.Time) bool {
	end := ev.EndsAt()
	return end != nil && !end.Add(c.Grace).After(now)
}

// Held reports whether t still has its holder: an erased member's Ticket
// is detached and an erased guest's has no address. A Ticket without one
// has no page and no Ticket QR.
func Held(t Ticket) bool {
	return held(t)
}
