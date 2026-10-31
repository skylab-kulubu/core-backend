package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/mail"
	"github.com/skylab-kulubu/core-backend/internal/membershipterm"
	"github.com/skylab-kulubu/core-backend/internal/season"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

type termsRunner struct {
	run  membershipterm.Run
	err  error
	opts membershipterm.RunOptions
}

func (r *termsRunner) Run(_ context.Context, opts membershipterm.RunOptions) (membershipterm.Run, error) {
	r.opts = opts
	return r.run, r.err
}

// The dry run says, as numbers, what apply would write before anything is
// written.
func TestManagementTermsCommandPrintsCounts(t *testing.T) {
	t.Parallel()
	runner := &termsRunner{run: membershipterm.Run{Mode: membershipterm.ModeDryRun, SeasonFound: true, Backfilled: 23, Ended: 1, Mails: 0, Weekly: membershipterm.WeeklyNotDue, Status: "ok"}}
	var out bytes.Buffer
	if code := managementTermsCommand(context.Background(), runner, membershipterm.ModeDryRun, &out); code != 0 {
		t.Fatalf("exit %d\n%s", code, out.String())
	}
	if runner.opts.Mode != membershipterm.ModeDryRun || runner.opts.Trigger != membershipterm.TriggerCLI || runner.opts.OnlyIfDue {
		t.Fatalf("options %+v", runner.opts)
	}
	text := out.String()
	for _, want := range []string{"terms added (memberships without one, would write)  23", "terms ended (membership gone)", "status"} {
		if !strings.Contains(text, want) {
			t.Fatalf("output lacks %q:\n%s", want, text)
		}
	}

	locked := &termsRunner{err: membershipterm.ErrLocked}
	if code := managementTermsCommand(context.Background(), locked, membershipterm.ModeApply, &out); code != managementTermsExitLocked {
		t.Fatalf("locked exit %d", code)
	}
	failed := &termsRunner{run: membershipterm.Run{Status: "failed", ErrorCode: "warn_failed"}, err: errors.New("x")}
	out.Reset()
	if code := managementTermsCommand(context.Background(), failed, membershipterm.ModeApply, &out); code != 1 || !strings.Contains(out.String(), "warn_failed") {
		t.Fatalf("failed exit %d\n%s", code, out.String())
	}
}

// --apply needs the mode to be apply: terms written while the job is off
// would not be kept.
func TestManagementTermsCommandApplyNeedsTheMode(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	env := map[string]string{"MANAGEMENT_TERMS_MODE": "dry-run"}
	if code := runManagementTerms([]string{"--apply"}, func(k string) string { return env[k] }, &out); code != 2 || !strings.Contains(out.String(), "MANAGEMENT_TERMS_MODE=apply") {
		t.Fatalf("exit %d\n%s", code, out.String())
	}
	out.Reset()
	if code := runManagementTerms([]string{"extra"}, func(string) string { return "" }, &out); code != 2 {
		t.Fatalf("an argument: exit %d", code)
	}
	out.Reset()
	if code := runManagementTerms(nil, func(k string) string { return map[string]string{"MANAGEMENT_TERMS_MODE": "on"}[k] }, &out); code != 2 {
		t.Fatalf("a bad mode: exit %d", code)
	}
}

// SKYMAIL_MANAGEMENT_TERM_TEMPLATE_KEY: unset takes the seeded key, empty
// turns the mails off.
func TestManagementTermKeyFromEnv(t *testing.T) {
	t.Parallel()
	sky := &mail.SkyMail{}
	membershipTemplateKeys(sky, func(string) (string, bool) { return "", false })
	if sky.ManagementTermTemplateKey != "club.management-term" || !sky.ManagementTermConfigured() {
		t.Fatalf("default %q", sky.ManagementTermTemplateKey)
	}
	membershipTemplateKeys(sky, func(name string) (string, bool) { return "", name == "SKYMAIL_MANAGEMENT_TERM_TEMPLATE_KEY" })
	if sky.ManagementTermConfigured() {
		t.Fatal("an empty key is on")
	}
}

// Only apply hears of the membership routes' adds: off and dry-run join no
// notifier, so an add reads no Group for the terms.
func TestManagementTermsNotifyOnlyInApply(t *testing.T) {
	t.Parallel()
	deps := managementTermsDeps{dir: identity.NewMemory(), seasons: season.NewMemoryStore(), users: user.NewMemoryStore(), audit: teamaudit.NewMemory()}
	for mode, want := range map[membershipterm.Mode]bool{membershipterm.ModeOff: false, membershipterm.ModeDryRun: false, membershipterm.ModeApply: true} {
		terms, err := newManagementTerms(mode, false, deps, t.Logf)
		if err != nil {
			t.Fatal(err)
		}
		if got := managementTermsNotifier(terms); (got != nil) != want {
			t.Fatalf("%s: notifier %v", mode, got)
		}
	}
	if managementTermsNotifier(nil) != nil {
		t.Fatal("a nil service notifies")
	}
}
