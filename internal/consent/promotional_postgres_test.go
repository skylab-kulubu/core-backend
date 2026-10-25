package consent_test

import (
	"context"
	"errors"
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
	// Signed in now, Lookup counts the mailbox's grant, so the products do
	// not show the box again. Ticked all the same (the signup screen), it
	// would record a second row, the account's own, beside it; any
	// withdrawal ends both (TestAWithdrawLinkEndsBothRowsOfAPersonsGrant).
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

// personalHolders stands for Keycloak's Personal e-mail search; an address
// it does not list fails, as Keycloak unreachable.
type personalHolders map[string][]uuid.UUID

func (p personalHolders) PersonalEmailHolders(_ context.Context, email string) ([]uuid.UUID, error) {
	if ids, ok := p[email]; ok {
		return ids, nil
	}
	return nil, errors.New("identity: personal e-mail search failed with status 503")
}

// One promotional grant per person (s6 §5.8), even when two rows record it:
// the member's own (the signup screen) and one a form recorded for their
// Personal e-mail, which core's row does not hold. The withdraw link of
// either row ends both, and nothing of another purpose. With Keycloak
// unreachable a link still ends what core reaches without it, so the mails
// the person clicked on stop.
func TestAWithdrawLinkEndsBothRowsOfAPersonsGrant(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	member := uuid.New()
	f.exec(t, `INSERT INTO users (id, email) VALUES ($1, 'm@example.com')`, member)
	if _, err := f.svc.Grant(ctx, consent.Grant{Purpose: consent.PurposeEventInvitations, UserID: member, Source: consent.SourceSelf}); err != nil {
		t.Fatal(err)
	}
	grantBoth := func() (own, form uuid.UUID) {
		t.Helper()
		account, err := f.svc.Grant(ctx, consent.Grant{Purpose: consent.PurposePromotional, UserID: member, Source: consent.SourceSignup, ClientID: "account-center"})
		if err != nil || !account.Created || account.Status != consent.StatusActive {
			t.Fatalf("own grant %+v %v", account, err)
		}
		address, err := f.svc.Grant(ctx, consent.Grant{Purpose: consent.PurposePromotional, Email: "Personal@Example.org",
			Source: consent.SourceForms, ClientID: "forms", EmailVerified: true})
		if err != nil || !address.Created || address.Status != consent.StatusActive {
			t.Fatalf("form grant %+v %v", address, err)
		}
		return account.ID, address.ID
	}
	withdraw := func(id uuid.UUID) {
		t.Helper()
		outcome, err := f.svc.Withdraw(ctx, tokenOf(t, f.svc.Config().WithdrawURL(id)), consent.WithdrawByOneClick)
		if err != nil || outcome != consent.OutcomeWithdrawn {
			t.Fatalf("withdraw %v %v", outcome, err)
		}
	}
	openPromotional := func() int {
		return f.count(t, `purpose = 'promotional' AND ended_at IS NULL`)
	}

	f.svc.SetAddresses(addressSource{member: {"m@example.com", "Personal@Example.org"}})
	f.svc.SetPersonalEmailHolders(personalHolders{"personal@example.org": {member}})
	own, _ := grantBoth()
	withdraw(own)
	if n := openPromotional(); n != 0 {
		t.Fatalf("the account's link left %d promotional rows open", n)
	}
	_, form := grantBoth()
	withdraw(form)
	if n := openPromotional(); n != 0 {
		t.Fatalf("the Personal e-mail's link left %d promotional rows open", n)
	}
	if f.count(t, `purpose = 'promotional' AND ended_via = 'one_click' AND email IS NULL`) != 4 {
		t.Fatal("the ended rows do not say how they ended, or keep an address")
	}
	if f.count(t, `purpose = 'event_invitations' AND ended_at IS NULL`) != 1 {
		t.Fatal("a promotional link ended another purpose's grant")
	}

	// Keycloak unreachable: each link ends its own row (and what core's row
	// reaches); the other row stays open.
	f.svc.SetAddresses(addressSource{})
	f.svc.SetPersonalEmailHolders(personalHolders{})
	for _, ownFirst := range []bool{true, false} {
		own, form = grantBoth()
		first, second := form, own
		if ownFirst {
			first, second = own, form
		}
		withdraw(first)
		if f.count(t, `id = $1 AND ended_at IS NOT NULL`, first) != 1 || f.count(t, `id = $1 AND ended_at IS NULL`, second) != 1 {
			t.Fatalf("without Keycloak (own first: %v)", ownFirst)
		}
		withdraw(second)
		if n := openPromotional(); n != 0 {
			t.Fatalf("without Keycloak (own first: %v) %d left open", ownFirst, n)
		}
	}
}
