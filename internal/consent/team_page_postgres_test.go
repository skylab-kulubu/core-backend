package consent_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/consent"
)

// A member's team page consent (s6 row 4 b, docs/team-page-consent.md) is
// their account's own: active at once, recorded for the web with its text
// and source, read by the public rosters (TeamPageDecisions), ended by the
// person without Keycloak and by staff on their mail, and kept as proof
// without being read any more. No mail route serves it.
func TestTeamPageConsentIsTheAccountsOwnAndTheRostersReadIt(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	member, other, erased := uuid.New(), uuid.New(), uuid.New()
	f.exec(t, `INSERT INTO users (id, email) VALUES ($1, 'm@example.com'), ($2, 'o@example.com'), ($3, 'e@example.com')`, member, other, erased)
	// Keycloak is down for everyone: withdrawing the team page consent does
	// not need the person's addresses.
	f.svc.SetAddresses(addressSource{})

	result, err := f.svc.Grant(ctx, consent.Grant{Purpose: consent.PurposeTeamPage, UserID: member, Source: consent.SourceSelf, ClientID: "account-center"})
	if err != nil || result.Status != consent.StatusActive || !result.Created {
		t.Fatalf("grant %+v %v", result, err)
	}
	if f.count(t, `id = $1 AND purpose = 'team_page' AND channel = 'web' AND source = 'self' AND client_id = 'account-center'
		AND text_version = 'ekip-sayfasi-v1' AND confirmed_via = 'account' AND email IS NULL`, result.ID) != 1 {
		t.Fatal("the grant is not stored as the account's own, for the web")
	}
	if len(f.mail.all()) != 0 {
		t.Fatal("a team page grant mailed a confirmation")
	}
	if _, err := f.svc.Grant(ctx, consent.Grant{Purpose: consent.PurposeTeamPage, UserID: erased, Source: consent.SourceSelf}); err != nil {
		t.Fatal(err)
	}
	f.exec(t, `UPDATE users SET account_state = 'deletion_pending', deletion_requested_at = now() WHERE id = $1`, erased)

	shown, err := f.svc.TeamPageDecisions(ctx, []uuid.UUID{member, other, erased})
	if err != nil || !shown[member] || len(shown) != 1 {
		t.Fatalf("decisions %v %v", shown, err)
	}

	for name, call := range map[string]func() error{
		"audience": func() error { _, _, err := f.svc.Audience(ctx, consent.PurposeTeamPage, uuid.Nil, 10); return err },
		"lookup": func() error {
			_, err := f.svc.Lookup(ctx, consent.PurposeTeamPage, []string{"m@example.com"})
			return err
		},
		"renewal": func() error {
			_, err := f.svc.RequestRenewal(ctx, consent.PurposeTeamPage, []uuid.UUID{result.ID})
			return err
		},
		"product withdrawal": func() error {
			_, err := f.svc.WithdrawForAddress(ctx, consent.PurposeTeamPage, "m@example.com")
			return err
		},
	} {
		if err := call(); !errors.Is(err, consent.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if f.count(t, `ended_at IS NULL AND user_id = $1`, member) != 1 {
		t.Fatal("a mail route changed the grant")
	}

	n, err := f.svc.WithdrawMine(ctx, member, consent.PurposeTeamPage)
	if err != nil || n != 1 {
		t.Fatalf("withdraw %d %v", n, err)
	}
	// Withdrawn is a decision of its own: false, not absent.
	if shown, err := f.svc.TeamPageDecisions(ctx, []uuid.UUID{member, other}); err != nil || len(shown) != 1 {
		t.Fatalf("decisions after withdrawal %v %v", shown, err)
	} else if open, decided := shown[member]; !decided || open {
		t.Fatalf("withdrawn member %v %v", open, decided)
	}
	if f.count(t, `id = $1 AND ended_reason = 'withdrawn' AND ended_via = 'self'`, result.ID) != 1 {
		t.Fatal("the withdrawn grant is not kept as proof")
	}

	// Given again: open again, the ended row stays as proof.
	if _, err := f.svc.Grant(ctx, consent.Grant{Purpose: consent.PurposeTeamPage, UserID: member, Source: consent.SourceSelf}); err != nil {
		t.Fatal(err)
	}
	if shown, err := f.svc.TeamPageDecisions(ctx, []uuid.UUID{member}); err != nil || !shown[member] {
		t.Fatalf("given again %v %v", shown, err)
	}
	if _, err := f.svc.WithdrawMine(ctx, member, consent.PurposeTeamPage); err != nil {
		t.Fatal(err)
	}

	// Given again, then withdrawn by mail to info@ (s6 §6).
	f.svc.SetAddresses(addressSource{member: {"m@example.com"}})
	f.svc.SetPersonalEmailHolders(personalHolders{"m@example.com": nil})
	if _, err := f.svc.Grant(ctx, consent.Grant{Purpose: consent.PurposeTeamPage, UserID: member, Source: consent.SourceSelf}); err != nil {
		t.Fatal(err)
	}
	ended, err := f.svc.WithdrawByStaff(ctx, []consent.Purpose{consent.PurposeTeamPage}, "M@example.com")
	if err != nil || ended[consent.PurposeTeamPage] != 1 {
		t.Fatalf("staff %v %v", ended, err)
	}
}

// With consents off (CONTACT_CONSENT_KEY unset) nobody can withdraw, so the
// rosters act on no grant.
func TestTeamPageAccountsWithConsentsOffIsNobody(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	member := uuid.New()
	f.exec(t, `INSERT INTO users (id, email) VALUES ($1, 'm@example.com')`, member)
	if _, err := f.svc.Grant(ctx, consent.Grant{Purpose: consent.PurposeTeamPage, UserID: member, Source: consent.SourceSelf}); err != nil {
		t.Fatal(err)
	}
	off := consent.NewService(f.pool, consent.Config{}, nil)
	shown, err := off.TeamPageDecisions(ctx, []uuid.UUID{member})
	if err != nil || len(shown) != 0 {
		t.Fatalf("off %v %v", shown, err)
	}
}
