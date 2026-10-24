package consent_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/consent"
)

// The promotional consent (s6 §5.8): a form filled without an account binds
// it to the address the person typed, and it waits for the confirmation
// link; once the person opens an account with that address it is the
// account's (the account's list, Lookup and withdrawal reach it). It never
// asks to renew, and once withdrawn its proof stays without the address.
func TestPromotionalGrantFollowsTheAddressIntoTheAccountAndNeverRenews(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	result, err := f.svc.Grant(ctx, consent.Grant{Purpose: consent.PurposePromotional, Email: "Ada@Example.com", Source: consent.SourceForms, ClientID: "forms"})
	if err != nil || result.Status != consent.StatusPending || !result.Created {
		t.Fatalf("form grant %+v %v", result, err)
	}
	mails := f.mail.all()
	if len(mails) != 1 || mails[0].vars["purpose"] != "promotional" {
		t.Fatalf("confirmation mail %+v", mails)
	}
	if f.count(t, `id = $1 AND purpose = 'promotional' AND source = 'forms' AND text_version = 'tanitim-v1' AND email = 'ada@example.com'`, result.ID) != 1 {
		t.Fatal("form grant not stored as expected")
	}
	if outcome, err := f.svc.Confirm(ctx, tokenOf(t, mails[0].vars["confirmUrl"])); err != nil || outcome != consent.OutcomeConfirmed {
		t.Fatalf("confirm %v %v", outcome, err)
	}

	member := uuid.New()
	f.exec(t, `INSERT INTO users (id, email) VALUES ($1, 'ada@example.com')`, member)
	own, err := f.svc.Mine(ctx, member)
	if err != nil || len(own) != 1 || own[0].Purpose != consent.PurposePromotional || own[0].Status != consent.StatusActive || own[0].Source != consent.SourceForms {
		t.Fatalf("mine %+v %v", own, err)
	}
	// Signed in now, the box is not shown again; ticking it on the signup
	// screen would keep the open grant of the mailbox as it is.
	if states, err := f.svc.Lookup(ctx, consent.PurposePromotional, []string{"ada@example.com"}); err != nil || states["ada@example.com"] != consent.StatusActive {
		t.Fatalf("lookup %v %v", states, err)
	}

	f.now = f.now.Add(consent.RenewalAfter + consent.RenewalAnswerWindow + 24*time.Hour)
	entries, _, err := f.svc.Audience(ctx, consent.PurposePromotional, uuid.Nil, 10)
	if err != nil || len(entries) != 1 {
		t.Fatalf("audience %+v %v", entries, err)
	}
	if entry := entries[0]; entry.RenewalDue || entry.RenewURL != "" || entry.TextVersion != "tanitim-v1" || entry.Email != "ada@example.com" {
		t.Fatalf("entry %+v", entry)
	}
	if n, err := f.svc.RequestRenewal(ctx, consent.PurposePromotional, []uuid.UUID{result.ID}); err != nil || n != 0 {
		t.Fatalf("renewal of a promotional grant %d %v", n, err)
	}
	if f.count(t, `renewal_requested_at IS NOT NULL`) != 0 {
		t.Fatal("a promotional grant was asked to renew")
	}

	if n, err := f.svc.WithdrawMine(ctx, member, consent.PurposePromotional); err != nil || n != 1 {
		t.Fatalf("withdraw mine %d %v", n, err)
	}
	if f.count(t, `id = $1 AND ended_reason = 'withdrawn' AND ended_via = 'self' AND email IS NULL AND email_hmac IS NOT NULL`, result.ID) != 1 {
		t.Fatal("the withdrawn grant does not keep its proof without the address")
	}
}

// A person's own promotional grant keeps where they gave it; it is active
// at once, and account erasure deletes it with the rest.
func TestPromotionalGrantKeepsItsSourceAndGoesWithTheAccount(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	member := uuid.New()
	f.exec(t, `INSERT INTO users (id, email) VALUES ($1, 'm@example.com')`, member)
	result, err := f.svc.Grant(ctx, consent.Grant{Purpose: consent.PurposePromotional, UserID: member, Source: consent.SourceSignup, ClientID: "account-center"})
	if err != nil || result.Status != consent.StatusActive {
		t.Fatalf("signup grant %+v %v", result, err)
	}
	if f.count(t, `user_id = $1 AND purpose = 'promotional' AND source = 'signup' AND client_id = 'account-center' AND confirmed_via = 'account'`, member) != 1 {
		t.Fatal("signup grant not stored as expected")
	}
	if _, err := f.svc.Grant(ctx, consent.Grant{Purpose: consent.PurposeEventInvitations, UserID: member, Source: consent.SourceSelf}); err != nil {
		t.Fatal(err)
	}
	if n, err := f.svc.EraseSubject(ctx, member, []string{"m@example.com"}); err != nil || n != 2 {
		t.Fatalf("erased %d %v", n, err)
	}
	// Guest apply, Place and Guessr cannot give it.
	for _, source := range []consent.Source{consent.SourceGuestApply, consent.SourcePlace, consent.SourceGuessr} {
		if _, err := f.svc.Grant(ctx, consent.Grant{Purpose: consent.PurposePromotional, Email: "g@example.com", Source: source}); err == nil {
			t.Errorf("promotional grant from %s", source)
		}
	}
	if f.count(t, `true`) != 0 {
		t.Fatal("a refused grant was stored")
	}
}
