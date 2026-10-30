package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/mail"
	"github.com/skylab-kulubu/core-backend/internal/teammail"
	"github.com/skylab-kulubu/core-backend/internal/teampagenotice"
)

func TestTeamPageNoticeOptions(t *testing.T) {
	for _, tc := range []struct {
		args  []string
		apply bool
		code  int
	}{
		{args: nil},
		{args: []string{"-apply"}, apply: true},
		{args: []string{"--apply"}, apply: true},
		{args: []string{"-apply", "extra"}, code: 2},
		{args: []string{"-send"}, code: 2},
	} {
		var errOut bytes.Buffer
		apply, code := parseTeamPageNoticeOptions(tc.args, &errOut)
		if apply != tc.apply || code != tc.code {
			t.Errorf("%v: apply %v code %d (%s)", tc.args, apply, code, errOut.String())
		}
	}
}

// Without the transition's end, or with consents off, nothing runs: the
// configuration is checked before Keycloak, the database or SkyMail.
func TestTeamPageNoticeRefusesWithoutTheTransitionOrConsents(t *testing.T) {
	env := func(vars map[string]string) func(string) string {
		return func(name string) string { return vars[name] }
	}
	var out, errOut bytes.Buffer
	if code := runTeamPageNotice(nil, env(nil), &out, &errOut); code != 2 || !strings.Contains(errOut.String(), identity.TeamPageGraceUntilEnv) {
		t.Fatalf("no transition: %d %s", code, errOut.String())
	}
	errOut.Reset()
	if code := runTeamPageNotice(nil, env(map[string]string{identity.TeamPageGraceUntilEnv: "10.11.2026"}), &out, &errOut); code != 2 {
		t.Fatalf("not RFC 3339: %d %s", code, errOut.String())
	}
	errOut.Reset()
	if code := runTeamPageNotice(nil, env(map[string]string{identity.TeamPageGraceUntilEnv: "2026-11-10T21:00:00Z"}), &out, &errOut); code != teamPageNoticeExitOff || !strings.Contains(errOut.String(), "consents are off") {
		t.Fatalf("consents off: %d %s", code, errOut.String())
	}
	if out.Len() != 0 {
		t.Fatalf("printed %q", out.String())
	}
}

type noticePeople map[uuid.UUID]teammail.Recipient

func (p noticePeople) Recipient(_ context.Context, id uuid.UUID) (teammail.Recipient, teammail.SkipReason, error) {
	return p[id], "", nil
}

type noticeConsents struct{}

func (noticeConsents) TeamPageDecisions(context.Context, []uuid.UUID) (map[uuid.UUID]bool, error) {
	return map[uuid.UUID]bool{}, nil
}

type noticeMailer struct {
	err  error
	sent int
}

func (m *noticeMailer) TeamPageNotice(context.Context, string, string, map[string]string) error {
	if m.err != nil {
		return m.err
	}
	m.sent++
	return nil
}

type noticeLedger map[uuid.UUID]bool

func (l noticeLedger) Notified(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]bool, error) {
	return l, nil
}

func (l noticeLedger) Claim(_ context.Context, id uuid.UUID, _ time.Time) (bool, error) {
	if l[id] {
		return false, nil
	}
	l[id] = true
	return true, nil
}

func (l noticeLedger) Release(_ context.Context, id uuid.UUID) error {
	delete(l, id)
	return nil
}

// The record is counts only: no address or name is printed. A dry run
// sends nothing; an unknown template stops with exit 3; a passed transition
// with exit 2.
func TestTeamPageNoticeCommandPrintsCountsOnly(t *testing.T) {
	ada := uuid.New()
	sky := &noticeMailer{}
	cfg := teampagenotice.Config{
		Listings: func(context.Context) ([]identity.TeamPageListing, error) {
			return []identity.TeamPageListing{{Team: identity.Group{Name: "WEBLAB", Path: "/UYELER/ARGE/WEBLAB"}, People: []uuid.UUID{ada}}}, nil
		},
		Consents: noticeConsents{},
		People:   noticePeople{ada: {Email: "ada@example.test", FullName: "Ada Lovelace", FirstName: "Ada"}},
		Mail:     sky, Ledger: noticeLedger{},
		HideAt:      time.Now().Add(14 * 24 * time.Hour),
		SettingsURL: "https://my.yildizskylab.com/club-profile",
	}
	var out, errOut bytes.Buffer
	if code := teamPageNoticeCommand(context.Background(), &out, &errOut, false, cfg); code != 0 || sky.sent != 0 ||
		!strings.Contains(out.String(), "dry run") || !strings.Contains(out.String(), "would notify: 1") {
		t.Fatalf("dry run: %d sent %d\n%s%s", code, sky.sent, out.String(), errOut.String())
	}
	out.Reset()
	if code := teamPageNoticeCommand(context.Background(), &out, &errOut, true, cfg); code != 0 || sky.sent != 1 || !strings.Contains(out.String(), "sent: 1") {
		t.Fatalf("apply: %d sent %d\n%s%s", code, sky.sent, out.String(), errOut.String())
	}
	for _, leak := range []string{"ada@", "Ada", ada.String()} {
		if strings.Contains(out.String()+errOut.String(), leak) {
			t.Fatalf("the record names %q:\n%s%s", leak, out.String(), errOut.String())
		}
	}

	cfg.Ledger = noticeLedger{}
	cfg.Mail = &noticeMailer{err: &mail.SendError{Status: 404}}
	errOut.Reset()
	if code := teamPageNoticeCommand(context.Background(), &out, &errOut, true, cfg); code != teamPageNoticeExitOff || !strings.Contains(errOut.String(), "seed it") {
		t.Fatalf("no template: %d %s", code, errOut.String())
	}
	cfg.HideAt = time.Now().Add(-time.Hour)
	errOut.Reset()
	if code := teamPageNoticeCommand(context.Background(), &out, &errOut, true, cfg); code != 2 || !strings.Contains(errOut.String(), "has passed") {
		t.Fatalf("over: %d %s", code, errOut.String())
	}
}
