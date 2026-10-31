// Package teampagenotice sends the team page notice (docs/team-page-consent.md,
// ekip-yetki-devri 21): when the team page consent's transition starts
// (TEAM_PAGE_CONSENT_GRACE_UNTIL), every member a public team page lists
// who has not decided about the consent is told, once, that the pages will
// show only those who consent. One mail per person, however many pages list
// them; a ledger (team_page_notices) keeps anyone from getting it twice.
//
// It is a transactional mail (SkyMail's islem stream, KVKK art. 10): it tells
// a person of a change in how their own data is published. Nothing
// promotional goes into it.
package teampagenotice

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/mail"
	"github.com/skylab-kulubu/core-backend/internal/teammail"
)

// Listings reads the public rosters before consents
// (identity.TeamPageListings).
type Listings func(ctx context.Context) ([]identity.TeamPageListing, error)

// Consents answers what people decided about the team page consent: true
// an open grant, false withdrawn, absent never decided
// (consent.Service.TeamPageDecisions).
type Consents interface {
	TeamPageDecisions(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]bool, error)
}

// People reads whom a mail goes to (teammail.DirectoryPeople): Keycloak's
// primary e-mail and core's name; nobody erased, disabled or gone.
type People interface {
	Recipient(ctx context.Context, id uuid.UUID) (teammail.Recipient, teammail.SkipReason, error)
}

// Mailer sends the notice (mail.SkyMail).
type Mailer interface {
	TeamPageNotice(ctx context.Context, recipient, fullName string, vars map[string]string) error
}

// Ledger records who was sent the notice (PostgresLedger).
type Ledger interface {
	// Notified is which of ids have a row.
	Notified(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]bool, error)
	// Claim writes id's row before its mail goes; false when it has one
	// already (another run took it).
	Claim(ctx context.Context, id uuid.UUID, at time.Time) (bool, error)
	// Release deletes id's row: its mail surely did not go.
	Release(ctx context.Context, id uuid.UUID) error
}

// Config is what a run reads and writes.
type Config struct {
	Listings Listings
	Consents Consents
	People   People
	Mail     Mailer
	Ledger   Ledger
	// HideAt is the end of the transition (TEAM_PAGE_CONSENT_GRACE_UNTIL).
	HideAt time.Time
	// SettingsURL is Account Center's page with the switch.
	SettingsURL string
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

// Report counts what a run found and did. It names nobody.
type Report struct {
	// Rosters is how many public rosters were read.
	Rosters int
	// Listed is how many people they list, each once.
	Listed int
	// Consented, Withdrew and Undecided split Listed by their decision.
	Consented, Withdrew, Undecided int
	// AlreadyNotified is how many undecided people were sent the notice
	// before (or by another run meanwhile).
	AlreadyNotified int
	// Skipped is how many undecided people get no mail: erased, disabled
	// or gone, or without a primary e-mail.
	Skipped int
	// Due is how many would be (dry run) or were to be (apply) sent the
	// notice; DueBoard of them are on the Board's roster.
	Due, DueBoard int
	// Sent took, Failed surely did not (a later run sends them), Unknown
	// went out without an answer (SkyMail may have taken them; never sent
	// again).
	Sent, Failed, Unknown int
}

var (
	// ErrNoTransition: TEAM_PAGE_CONSENT_GRACE_UNTIL is not set, so there
	// is no date to tell of.
	ErrNoTransition = errors.New("teampagenotice: no transition (TEAM_PAGE_CONSENT_GRACE_UNTIL is not set)")
	// ErrTransitionOver: the transition has ended; the notice would tell of
	// the past.
	ErrTransitionOver = errors.New("teampagenotice: the transition has ended")
	// ErrTemplateMissing: SkyMail does not know the template
	// (core.team-page-consent-notice is not seeded).
	ErrTemplateMissing = errors.New("teampagenotice: SkyMail has no such template")
)

// releaseTimeout bounds taking a row out after a refused mail, which runs
// even when the run is being stopped.
const releaseTimeout = 10 * time.Second

// due is one person the notice goes to.
type due struct {
	id    uuid.UUID
	teams []string
	board bool
}

// Run reads who the notice goes to and, when apply, sends it. Without apply
// it writes and sends nothing: the report says how many it would send.
func Run(ctx context.Context, cfg Config, apply bool) (Report, error) {
	now := time.Now
	if cfg.Now != nil {
		now = cfg.Now
	}
	if cfg.HideAt.IsZero() {
		return Report{}, ErrNoTransition
	}
	if !now().Before(cfg.HideAt) {
		return Report{}, ErrTransitionOver
	}
	listings, err := cfg.Listings(ctx)
	if err != nil {
		return Report{}, fmt.Errorf("public rosters: %w", err)
	}
	report := Report{Rosters: len(listings)}
	byID := map[uuid.UUID]*due{}
	var order []uuid.UUID
	for _, listing := range listings {
		name := teamName(listing.Team)
		for _, id := range listing.People {
			d, ok := byID[id]
			if !ok {
				d = &due{id: id}
				byID[id] = d
				order = append(order, id)
			}
			if !contains(d.teams, name) {
				d.teams = append(d.teams, name)
			}
			d.board = d.board || listing.Board
		}
	}
	report.Listed = len(order)
	decisions, err := cfg.Consents.TeamPageDecisions(ctx, order)
	if err != nil {
		return report, fmt.Errorf("team page consents: %w", err)
	}
	var undecided []uuid.UUID
	for _, id := range order {
		open, decided := decisions[id]
		switch {
		case !decided:
			undecided = append(undecided, id)
		case open:
			report.Consented++
		default:
			report.Withdrew++
		}
	}
	report.Undecided = len(undecided)
	notified, err := cfg.Ledger.Notified(ctx, undecided)
	if err != nil {
		return report, fmt.Errorf("ledger: %w", err)
	}
	hideAt := hideAtText(cfg.HideAt)
	for _, id := range undecided {
		if notified[id] {
			report.AlreadyNotified++
			continue
		}
		recipient, skip, err := cfg.People.Recipient(ctx, id)
		if err != nil {
			return report, fmt.Errorf("recipient: %w", err)
		}
		if skip != "" {
			report.Skipped++
			continue
		}
		d := byID[id]
		report.Due++
		if d.board {
			report.DueBoard++
		}
		if !apply {
			continue
		}
		if err := send(ctx, cfg, now(), d, recipient, hideAt, &report); err != nil {
			return report, err
		}
	}
	return report, nil
}

// send claims d's row, sends, and keeps the row unless the mail surely did
// not go. An error stops the run: SkyMail has no template, or the ledger
// cannot be written.
func send(ctx context.Context, cfg Config, at time.Time, d *due, to teammail.Recipient, hideAt string, report *Report) error {
	claimed, err := cfg.Ledger.Claim(ctx, d.id, at)
	if err != nil {
		return fmt.Errorf("ledger: %w", err)
	}
	if !claimed {
		report.Due--
		if d.board {
			report.DueBoard--
		}
		report.AlreadyNotified++
		return nil
	}
	board := ""
	if d.board {
		board = "true"
	}
	vars := map[string]string{
		"FirstName":   to.FirstName,
		"Teams":       strings.Join(d.teams, ", "),
		"HideAt":      hideAt,
		"SettingsURL": cfg.SettingsURL,
		"Board":       board,
	}
	err = cfg.Mail.TeamPageNotice(ctx, to.Email, to.FullName, vars)
	switch {
	case err == nil:
		report.Sent++
		return nil
	case errors.Is(err, mail.ErrMaybeDelivered):
		// The row stays: SkyMail may have taken it, and /single reads no
		// idempotency key, so a second try could mail the person twice.
		report.Unknown++
		return nil
	}
	// The mail surely did not go. A stopped run (ctx cancelled) still takes
	// the row out, or the person would never be told.
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
	defer cancel()
	if releaseErr := cfg.Ledger.Release(releaseCtx, d.id); releaseErr != nil {
		return fmt.Errorf("ledger: %w (the person's notice did not go and stays recorded)", releaseErr)
	}
	if errors.Is(err, mail.ErrTemplateUnconfigured) {
		return err
	}
	var refused *mail.SendError
	if errors.As(err, &refused) && refused.Status == http.StatusNotFound {
		// SkyMail has no such template: every other mail would be refused
		// the same way. Seed it, then run again.
		return fmt.Errorf("%w: %w", ErrTemplateMissing, err)
	}
	report.Failed++
	return nil
}

// teamName is how the notice names a team, as the membership mails do:
// its code and its Turkish display name, "WEBLAB · Web Geliştirme"; the
// code alone without one.
func teamName(group identity.Group) string {
	code := strings.TrimSpace(group.Name)
	if code == "" {
		code = group.Path[strings.LastIndex(group.Path, "/")+1:]
	}
	if turkish := strings.TrimSpace(group.Attributes["display_name_tr"]); turkish != "" && turkish != code {
		return code + " · " + turkish
	}
	return code
}

// hideAtText is the end of the transition as the mail shows it: Europe/
// Istanbul's wall clock, "11.11.2026 00:00".
func hideAtText(at time.Time) string {
	loc, err := time.LoadLocation("Europe/Istanbul")
	if err != nil {
		loc = time.FixedZone("TRT", 3*60*60)
	}
	return at.In(loc).Format("02.01.2006 15:04")
}

func contains(list []string, s string) bool {
	for _, have := range list {
		if have == s {
			return true
		}
	}
	return false
}
