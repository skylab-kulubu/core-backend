package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/commpref"
	"github.com/skylab-kulubu/core-backend/internal/consent"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/mail"
	"github.com/skylab-kulubu/core-backend/internal/teammail"
	"github.com/skylab-kulubu/core-backend/internal/teampagenotice"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// The team page notice (docs/team-page-consent.md, "The notice";
// ekip-yetki-devri 21): run once inside the core container on the first day
// of the transition, after TEAM_PAGE_CONSENT_GRACE_UNTIL is set and the
// SkyMail template is seeded (the binary is /app/core-backend, not on PATH):
//
//	./core-backend team-page-notice           # dry run: counts, sends nothing
//	./core-backend team-page-notice -apply    # sends
//
// It mails every member a public team page lists who has not decided about
// the team page consent, once; the ledger keeps a second run from mailing
// anyone again. It prints counts, never a person.
const teamPageNoticeCommandName = "team-page-notice"

// teamPageNoticeTemplateKeyEnv replaces the SkyMail template key; unset or
// empty, the seeded mail.DefaultTeamPageNoticeTemplateKey.
const teamPageNoticeTemplateKeyEnv = "SKYMAIL_TEAM_PAGE_NOTICE_TEMPLATE_KEY"

// teamPageNoticeSettingsPath is Account Center's page with the "Ekip
// sayfaları" switch (account-center #119).
const teamPageNoticeSettingsPath = "/club-profile"

// Exit codes beyond 0 (done), 1 (core, Keycloak or SkyMail failed, or some
// mails were refused: run it again; who got one is not mailed again) and 2
// (usage or configuration).
const teamPageNoticeExitOff = 3

func runTeamPageNotice(args []string, getenv func(string) string, out, errOut io.Writer) int {
	apply, code := parseTeamPageNoticeOptions(args, errOut)
	if code != 0 {
		return code
	}
	hideAt, err := identity.TeamPageGraceUntilFromEnv(getenv)
	if err != nil {
		fmt.Fprintf(errOut, "%s: %v\n", teamPageNoticeCommandName, err)
		return 2
	}
	if hideAt.IsZero() {
		fmt.Fprintf(errOut, "%s needs %s: the notice tells when the pages change\n", teamPageNoticeCommandName, identity.TeamPageGraceUntilEnv)
		return 2
	}
	consents, err := consent.ConfigFromEnv(getenv)
	if err != nil {
		fmt.Fprintf(errOut, "%s: %v\n", teamPageNoticeCommandName, err)
		return 2
	}
	if !consents.Enabled {
		fmt.Fprintf(errOut, "contact consents are off (%s is not set): nobody can give the team page consent the notice asks for\n", consent.KeyEnv)
		return teamPageNoticeExitOff
	}
	roots, err := authz.PrivilegedRootsFromEnv(getenv)
	if err != nil {
		fmt.Fprintf(errOut, "%s: %v\n", teamPageNoticeCommandName, err)
		return 2
	}
	keycloak, missing, err := keycloakFromEnv(getenv)
	if len(missing) > 0 {
		fmt.Fprintf(errOut, "%s needs %s: the rosters and the addresses are in Keycloak, and SkyMail takes core's token\n", teamPageNoticeCommandName, strings.Join(missing, ", "))
		return 2
	}
	if err != nil {
		fmt.Fprintf(errOut, "%s: %v\n", teamPageNoticeCommandName, err)
		return 2
	}
	if strings.TrimSpace(getenv("DATABASE_URL")) == "" {
		fmt.Fprintf(errOut, "%s needs DATABASE_URL\n", teamPageNoticeCommandName)
		return 2
	}
	center := strings.TrimSpace(getenv(commpref.AccountCenterURLEnv))
	if center == "" {
		center = commpref.DefaultAccountCenterURL
	}
	base, realm := keycloakRealmFromEnv(getenv)
	sky := newSkyMail(base, realm, keycloak.ClientID, keycloak.ClientSecret, getenv("SKYMAIL_URL"), skyMailTimeout)
	sky.TeamPageNoticeTemplateKey = mail.DefaultTeamPageNoticeTemplateKey
	if key := strings.TrimSpace(getenv(teamPageNoticeTemplateKeyEnv)); key != "" {
		sky.TeamPageNoticeTemplateKey = key
	}
	if notice := mail.DefaultOriginNotice(getenv("SKYMAIL_URL")); notice != "" {
		fmt.Fprintln(errOut, notice)
	}

	ctx, stop := commandContext()
	defer stop()
	pool, err := pgxpool.New(ctx, getenv("DATABASE_URL"))
	if err != nil {
		fmt.Fprintln(errOut, "database: cannot open the connection pool")
		return 1
	}
	defer pool.Close()
	dir := identity.NewKeycloak(keycloak)
	users := user.NewPostgresStore(pool)
	cfg := teampagenotice.Config{
		Listings: func(ctx context.Context) ([]identity.TeamPageListing, error) {
			return identity.TeamPageListings(ctx, dir, users, roots)
		},
		Consents:    consent.NewService(pool, consents, nil),
		People:      teammail.DirectoryPeople{Directory: dir, Accounts: users},
		Mail:        sky,
		Ledger:      teampagenotice.NewPostgresLedger(pool),
		HideAt:      hideAt,
		SettingsURL: strings.TrimRight(center, "/") + teamPageNoticeSettingsPath,
	}
	return teamPageNoticeCommand(ctx, out, errOut, apply, cfg)
}

// parseTeamPageNoticeOptions reads -apply; without it the run is dry.
func parseTeamPageNoticeOptions(args []string, errOut io.Writer) (bool, int) {
	flags := flag.NewFlagSet(teamPageNoticeCommandName, flag.ContinueOnError)
	flags.SetOutput(errOut)
	apply := flags.Bool("apply", false, "send the notice; without it nothing is sent or recorded")
	if err := flags.Parse(args); err != nil {
		return false, 2
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(errOut, "%s takes no argument but -apply\n", teamPageNoticeCommandName)
		return false, 2
	}
	return *apply, 0
}

// teamPageNoticeCommand runs the notice and prints its record: counts only.
func teamPageNoticeCommand(ctx context.Context, out, errOut io.Writer, apply bool, cfg teampagenotice.Config) int {
	report, err := teampagenotice.Run(ctx, cfg, apply)
	switch {
	case errors.Is(err, teampagenotice.ErrTransitionOver):
		fmt.Fprintf(errOut, "%s (%s) has passed: the notice would tell of the past; nothing sent\n", identity.TeamPageGraceUntilEnv, cfg.HideAt.Format(time.RFC3339))
		return 2
	case errors.Is(err, teampagenotice.ErrTemplateMissing), errors.Is(err, mail.ErrTemplateUnconfigured):
		writeTeamPageNoticeRecord(out, apply, cfg.HideAt, report)
		fmt.Fprintf(errOut, "skymail: no template %s; seed it (skymail-frontend emails:seed), then run again\n", mail.DefaultTeamPageNoticeTemplateKey)
		return teamPageNoticeExitOff
	case err != nil:
		writeTeamPageNoticeRecord(out, apply, cfg.HideAt, report)
		fmt.Fprintf(errOut, "%v; run it again (who got the notice does not get it again)\n", err)
		return 1
	}
	writeTeamPageNoticeRecord(out, apply, cfg.HideAt, report)
	if report.Failed > 0 {
		fmt.Fprintln(errOut, "SkyMail refused some notices; run it again to send them")
		return 1
	}
	return 0
}

func writeTeamPageNoticeRecord(out io.Writer, apply bool, hideAt time.Time, r teampagenotice.Report) {
	if apply {
		fmt.Fprintln(out, "team page notice")
	} else {
		fmt.Fprintln(out, "team page notice: dry run, nothing sent or recorded (-apply sends)")
	}
	fmt.Fprintf(out, "pages change at: %s\n", hideAt.Format(time.RFC3339))
	fmt.Fprintf(out, "public rosters: %d\n", r.Rosters)
	fmt.Fprintf(out, "listed people: %d (consented %d, withdrew %d, undecided %d)\n", r.Listed, r.Consented, r.Withdrew, r.Undecided)
	fmt.Fprintf(out, "undecided already notified: %d\n", r.AlreadyNotified)
	fmt.Fprintf(out, "undecided not written to (erased, disabled, no address): %d\n", r.Skipped)
	if apply {
		fmt.Fprintf(out, "to notify: %d (on the Board's roster %d)\n", r.Due, r.DueBoard)
		fmt.Fprintf(out, "sent: %d\n", r.Sent)
		fmt.Fprintf(out, "refused, sent on the next run: %d\n", r.Failed)
		fmt.Fprintf(out, "no answer, never sent again: %d\n", r.Unknown)
		return
	}
	fmt.Fprintf(out, "would notify: %d (on the Board's roster %d)\n", r.Due, r.DueBoard)
}
