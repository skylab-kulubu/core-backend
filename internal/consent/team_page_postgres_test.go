package consent_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/consent"
)

// A member's team page objection (s6 row 4 b, docs/team-page-consent.md) is
// their account's own: active at once, recorded for the web with its text
// and source, read by the public rosters (TeamPageDecisions), lifted by the
// person without Keycloak, and kept as proof without being read any more.
// No mail route serves it.
func TestTeamPageObjectionIsTheAccountsOwnAndTheRostersReadIt(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	member, other, erased := uuid.New(), uuid.New(), uuid.New()
	f.exec(t, `INSERT INTO users (id, email) VALUES ($1, 'm@example.com'), ($2, 'o@example.com'), ($3, 'e@example.com')`, member, other, erased)
	// Keycloak is down for everyone: the objection does not need the
	// person's addresses.
	f.svc.SetAddresses(addressSource{})

	if shown, err := f.svc.TeamPageDecisions(ctx, []uuid.UUID{member, other, erased}); err != nil || len(shown) != 0 {
		t.Fatalf("nobody objects %v %v", shown, err)
	}
	result, err := f.svc.Grant(ctx, consent.Grant{Purpose: consent.PurposeTeamPageObjection, UserID: member, Source: consent.SourceSelf, ClientID: "account-center"})
	if err != nil || result.Status != consent.StatusActive || !result.Created {
		t.Fatalf("object %+v %v", result, err)
	}
	if f.count(t, `id = $1 AND purpose = 'team_page_objection' AND channel = 'web' AND source = 'self' AND client_id = 'account-center'
		AND text_version = 'ekip-sayfasi-itiraz-v1' AND confirmed_via = 'account' AND email IS NULL`, result.ID) != 1 {
		t.Fatal("the objection is not stored as the account's own, for the web")
	}
	if len(f.mail.all()) != 0 {
		t.Fatal("an objection mailed a confirmation")
	}
	if _, err := f.svc.Grant(ctx, consent.Grant{Purpose: consent.PurposeTeamPageObjection, UserID: erased, Source: consent.SourceSelf}); err != nil {
		t.Fatal(err)
	}
	f.exec(t, `UPDATE users SET account_state = 'deletion_pending', deletion_requested_at = now() WHERE id = $1`, erased)

	shown, err := f.svc.TeamPageDecisions(ctx, []uuid.UUID{member, other, erased})
	if visible, objected := shown[member]; err != nil || !objected || visible || len(shown) != 1 {
		t.Fatalf("decisions %v %v", shown, err)
	}

	for name, call := range map[string]func() error{
		"audience": func() error {
			_, _, err := f.svc.Audience(ctx, consent.PurposeTeamPageObjection, uuid.Nil, 10)
			return err
		},
		"lookup": func() error {
			_, err := f.svc.Lookup(ctx, consent.PurposeTeamPageObjection, []string{"m@example.com"})
			return err
		},
		"renewal": func() error {
			_, err := f.svc.RequestRenewal(ctx, consent.PurposeTeamPageObjection, []uuid.UUID{result.ID})
			return err
		},
		"product withdrawal": func() error {
			_, err := f.svc.WithdrawForAddress(ctx, consent.PurposeTeamPageObjection, "m@example.com")
			return err
		},
	} {
		if err := call(); !errors.Is(err, consent.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if f.count(t, `ended_at IS NULL AND user_id = $1`, member) != 1 {
		t.Fatal("a mail route changed the objection")
	}

	// Lifted (the switch turned on again): shown at once, the row kept.
	n, err := f.svc.WithdrawMine(ctx, member, consent.PurposeTeamPageObjection)
	if err != nil || n != 1 {
		t.Fatalf("lift %d %v", n, err)
	}
	if shown, err := f.svc.TeamPageDecisions(ctx, []uuid.UUID{member, other}); err != nil || len(shown) != 0 {
		t.Fatalf("decisions after lifting %v %v", shown, err)
	}
	if f.count(t, `id = $1 AND ended_reason = 'withdrawn' AND ended_via = 'self'`, result.ID) != 1 {
		t.Fatal("the lifted objection is not kept as proof")
	}

	// Objecting again: hidden again, the ended row stays as proof.
	if _, err := f.svc.Grant(ctx, consent.Grant{Purpose: consent.PurposeTeamPageObjection, UserID: member, Source: consent.SourceSelf}); err != nil {
		t.Fatal(err)
	}
	if shown, err := f.svc.TeamPageDecisions(ctx, []uuid.UUID{member}); err != nil || len(shown) != 1 || shown[member] {
		t.Fatalf("objected again %v %v", shown, err)
	}
	if f.count(t, `user_id = $1 AND purpose = 'team_page_objection'`, member) != 2 {
		t.Fatal("the history is not kept")
	}
}

// The team_page consent of 2026-10-27 is no longer taken nor withdrawn. A
// row of it kept as proof changes nothing: an open one is shown anyway, and
// an open objection hides the person whatever their old consent said.
func TestTheOldTeamPageConsentIsNoLongerTaken(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	member, objector := uuid.New(), uuid.New()
	f.exec(t, `INSERT INTO users (id, email) VALUES ($1, 'm@example.com'), ($2, 'x@example.com')`, member, objector)
	if _, err := f.svc.Grant(ctx, consent.Grant{Purpose: consent.PurposeTeamPage, UserID: member, Source: consent.SourceSelf}); !errors.Is(err, consent.ErrPurposeNotEnabled) {
		t.Fatalf("grant %v", err)
	}
	if _, err := f.svc.WithdrawMine(ctx, member, consent.PurposeTeamPage); !errors.Is(err, consent.ErrPurposeNotEnabled) {
		t.Fatalf("withdraw %v", err)
	}
	for _, id := range []uuid.UUID{member, objector} {
		f.exec(t, `INSERT INTO contact_consents (id, purpose, channel, user_id, source, client_id, text_version, granted_at, confirmed_at, confirmed_via)
			VALUES ($1, 'team_page', 'web', $2, 'self', 'account-center', 'ekip-sayfasi-v1', now(), now(), 'account')`, uuid.New(), id)
	}
	if _, err := f.svc.Grant(ctx, consent.Grant{Purpose: consent.PurposeTeamPageObjection, UserID: objector, Source: consent.SourceSelf}); err != nil {
		t.Fatal(err)
	}
	shown, err := f.svc.TeamPageDecisions(ctx, []uuid.UUID{member, objector})
	if visible, objected := shown[objector]; err != nil || len(shown) != 1 || !objected || visible {
		t.Fatalf("decisions %v %v", shown, err)
	}
	own, err := f.svc.Mine(ctx, member)
	if err != nil || len(own) != 1 || own[0].Purpose != consent.PurposeTeamPage || own[0].Status != consent.StatusActive {
		t.Fatalf("the old consent is not listed as proof: %+v %v", own, err)
	}
}

// With consents off (CONTACT_CONSENT_KEY unset) nobody can lift an
// objection, so the rosters keep reading them: who objected stays hidden.
func TestTeamPageObjectionsHoldWithConsentsOff(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	member := uuid.New()
	f.exec(t, `INSERT INTO users (id, email) VALUES ($1, 'm@example.com')`, member)
	if _, err := f.svc.Grant(ctx, consent.Grant{Purpose: consent.PurposeTeamPageObjection, UserID: member, Source: consent.SourceSelf}); err != nil {
		t.Fatal(err)
	}
	off := consent.NewService(f.pool, consent.Config{}, nil)
	shown, err := off.TeamPageDecisions(ctx, []uuid.UUID{member})
	if visible, objected := shown[member]; err != nil || !objected || visible {
		t.Fatalf("off %v %v", shown, err)
	}
}
