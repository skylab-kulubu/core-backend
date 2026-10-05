package consent_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/consent"
)

// Whoever types an address cannot use core to flood it: one mail when the
// pending grant is recorded, then at most one a day when it is given again,
// three in all; and no mail carries a name.
func TestConfirmationMailIsCappedAtOneADayAndThreeInAll(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	grant := consent.Grant{Purpose: consent.PurposeEventInvitations, Email: "target@example.com", Source: consent.SourceGuestApply}
	give := func() {
		t.Helper()
		if _, err := f.svc.Grant(ctx, grant); err != nil {
			t.Fatal(err)
		}
	}
	give()
	for range 50 {
		give()
	}
	if got := len(f.mail.all()); got != 1 {
		t.Fatalf("%d mails within the first day, want 1", got)
	}
	f.now = f.now.Add(consent.ConfirmationResend - time.Minute)
	give()
	if got := len(f.mail.all()); got != 1 {
		t.Fatalf("%d mails before a day passed, want 1", got)
	}
	for day := 1; day <= 10; day++ {
		f.now = f.now.Add(consent.ConfirmationResend)
		give()
		give()
	}
	if got := len(f.mail.all()); got != consent.MaxConfirmationMails {
		t.Fatalf("%d mails over ten days, want %d", got, consent.MaxConfirmationMails)
	}
	if f.count(t, `confirmation_mails = 3 AND ended_at IS NULL`) != 1 || f.count(t, `true`) != 1 {
		t.Fatal("the pending row does not count its mails")
	}
	for _, mail := range f.mail.all() {
		for name, value := range mail.vars {
			if name != "confirmUrl" && name != "withdrawUrl" && name != "purpose" {
				t.Fatalf("the mail carries %s=%q", name, value)
			}
		}
	}
}

// A grant from a client not allowed to vouch for addresses waits for the
// person's confirmation, whatever it says about the address.
func TestOnlyAllowedClientsMayAssertAVerifiedAddress(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	strict := consent.NewService(f.pool, consent.TestConfig(testKey, "https://api.example.test/"), f.mail)
	strict.SetAsync(func(fn func()) { fn() })
	result, err := strict.Grant(ctx, consent.Grant{Purpose: consent.PurposeEventInvitations, Email: "p@example.com",
		Source: consent.SourcePlace, ClientID: "place", EmailVerified: true})
	if err != nil || result.Status != consent.StatusPending {
		t.Fatalf("unlisted client's verified grant %+v %v", result, err)
	}
	if len(f.mail.all()) != 1 || f.count(t, `confirmed_at IS NULL AND email = 'p@example.com'`) != 1 {
		t.Fatal("the grant did not wait for the person's confirmation")
	}
	// Listed clients only; another client's claim is not believed.
	placeOnly := consent.NewService(f.pool, consent.TestConfig(testKey, "https://api.example.test/", "place"), f.mail)
	if result, err := placeOnly.Grant(ctx, consent.Grant{Purpose: consent.PurposeEventInvitations, Email: "g@example.com",
		Source: consent.SourceGuessr, ClientID: "guessr", EmailVerified: true}); err != nil || result.Status != consent.StatusPending {
		t.Fatalf("guessr's claim under a place-only list %+v %v", result, err)
	}
	if result, err := placeOnly.Grant(ctx, consent.Grant{Purpose: consent.PurposeEventInvitations, Email: "q@example.com",
		Source: consent.SourcePlace, ClientID: "place", EmailVerified: true}); err != nil || result.Status != consent.StatusActive {
		t.Fatalf("place's claim under a place-only list %+v %v", result, err)
	}
}

// A verified grant that meets a pending one does not inherit its proof: the
// pending row ends as superseded (keeping its own evidence), and the verified
// request is a row of its own with its source, client, event and text. The
// pending row's links still do what they say.
func TestVerifiedGrantSupersedesAPendingOneWithItsOwnEvidence(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	eventID := uuid.New()
	f.exec(t, `INSERT INTO events (id, name, location, owner_team) VALUES ($1, 'Hack', 'YTÜ', 'WEBLAB')`, eventID)
	pending, err := f.svc.Grant(ctx, consent.Grant{Purpose: consent.PurposeEventInvitations, Email: "v@example.com",
		Source: consent.SourceGuestApply, EventID: &eventID})
	if err != nil || pending.Status != consent.StatusPending {
		t.Fatalf("pending %+v %v", pending, err)
	}
	mail := f.mail.all()[0]
	verified, err := f.svc.Grant(ctx, consent.Grant{Purpose: consent.PurposeEventInvitations, Email: "v@example.com",
		Source: consent.SourcePlace, ClientID: "place", EmailVerified: true})
	if err != nil || verified.Status != consent.StatusActive || !verified.Created || verified.ID == pending.ID {
		t.Fatalf("verified %+v %v", verified, err)
	}
	if f.count(t, `id = $1 AND source = 'guest_apply' AND event_id = $2 AND ended_reason = 'superseded' AND ended_via = 'service'
		AND confirmed_at IS NULL AND email IS NULL AND email_hmac IS NOT NULL`, pending.ID, eventID) != 1 {
		t.Fatal("the pending row did not end as superseded with its own evidence")
	}
	if f.count(t, `id = $1 AND source = 'place' AND client_id = 'place' AND event_id IS NULL AND text_version = 'davet-v1'
		AND confirmed_via = 'service' AND ended_at IS NULL AND email = 'v@example.com'`, verified.ID) != 1 {
		t.Fatal("the verified grant is not a row of its own evidence")
	}
	if outcome, err := f.svc.Confirm(ctx, tokenOf(t, mail.vars["confirmUrl"])); err != nil || outcome != consent.OutcomeSuperseded {
		t.Fatalf("confirm link of the superseded row %v %v", outcome, err)
	}
	if f.count(t, `id = $1 AND confirmed_at IS NULL`, pending.ID) != 1 {
		t.Fatal("the confirm link changed the superseded row")
	}
	if outcome, err := f.svc.Withdraw(ctx, tokenOf(t, mail.vars["withdrawUrl"]), consent.WithdrawByPage); err != nil || outcome != consent.OutcomeWithdrawn {
		t.Fatalf("withdraw link of the superseded row %v %v", outcome, err)
	}
	if f.count(t, `ended_at IS NULL`) != 0 {
		t.Fatal("the superseded row's withdraw link left the verified grant open")
	}
}

// Many concurrent grants for one address and one account: one open row
// each, one mail, and no caller sees an error. Grants racing withdrawals
// never leave two open rows.
func TestConcurrentGrantsKeepOneOpenRowAndNoCallerSeesAnError(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	member := uuid.New()
	f.exec(t, `INSERT INTO users (id, email) VALUES ($1, 'c@example.com')`, member)
	var wg sync.WaitGroup
	errs := make(chan error, 400)
	for range 32 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if _, err := f.svc.Grant(ctx, consent.Grant{Purpose: consent.PurposeEventInvitations, Email: "race@example.com", Source: consent.SourceGuestApply}); err != nil {
				errs <- err
			}
		}()
		go func() {
			defer wg.Done()
			if _, err := f.svc.Grant(ctx, consent.Grant{Purpose: consent.PurposeEventInvitations, UserID: member, Source: consent.SourceSelf}); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	if f.count(t, `ended_at IS NULL AND email = 'race@example.com'`) != 1 || f.count(t, `ended_at IS NULL AND user_id = $1`, member) != 1 {
		t.Fatal("concurrent grants left more or fewer than one open row per subject")
	}
	if got := len(f.mail.all()); got != 1 {
		t.Fatalf("%d confirmation mails for one pending grant", got)
	}

	for range 50 {
		wg.Add(3)
		go func() {
			defer wg.Done()
			if _, err := f.svc.Grant(ctx, consent.Grant{Purpose: consent.PurposeEventInvitations, Email: "flip@example.com", Source: consent.SourcePlace, ClientID: "place", EmailVerified: true}); err != nil {
				errs <- err
			}
		}()
		go func() {
			defer wg.Done()
			if _, err := f.svc.Grant(ctx, consent.Grant{Purpose: consent.PurposeEventInvitations, Email: "flip@example.com", Source: consent.SourceGuessr, ClientID: "guessr", EmailVerified: true}); err != nil {
				errs <- err
			}
		}()
		go func() {
			defer wg.Done()
			if _, err := f.svc.WithdrawForAddress(ctx, consent.PurposeEventInvitations, "flip@example.com"); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("grant or withdrawal error: %v", err)
	}
	if n := f.count(t, `ended_at IS NULL AND email = 'flip@example.com'`); n > 1 {
		t.Fatalf("%d open rows for one address", n)
	}
}

// A withdraw link from an invitation sent on an address grant that has since
// ended (only its HMAC is left) still ends the account's own grant that
// mails the same address.
func TestAnOldLinkOfAnEndedAddressGrantStillEndsTheAccountsOwnGrant(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	member := uuid.New()
	f.exec(t, `INSERT INTO users (id, email) VALUES ($1, 'Member@Example.com')`, member)
	old, err := f.svc.Grant(ctx, consent.Grant{Purpose: consent.PurposeEventInvitations, Email: "member@example.com",
		Source: consent.SourcePlace, ClientID: "place", EmailVerified: true})
	if err != nil {
		t.Fatal(err)
	}
	entries, _, err := f.svc.Audience(ctx, consent.PurposeEventInvitations, uuid.Nil, 10)
	if err != nil || len(entries) != 1 || entries[0].ID != old.ID {
		t.Fatalf("audience %+v %v", entries, err)
	}
	oldLink := tokenOf(t, entries[0].WithdrawURL)
	if _, err := f.svc.WithdrawForAddress(ctx, consent.PurposeEventInvitations, "member@example.com"); err != nil {
		t.Fatal(err)
	}
	// The person signs up and consents for their account.
	if _, err := f.svc.Grant(ctx, consent.Grant{Purpose: consent.PurposeEventInvitations, UserID: member, Source: consent.SourceSelf}); err != nil {
		t.Fatal(err)
	}
	if outcome, err := f.svc.Withdraw(ctx, oldLink, consent.WithdrawByOneClick); err != nil || outcome != consent.OutcomeWithdrawn {
		t.Fatalf("old link %v %v", outcome, err)
	}
	if f.count(t, `ended_at IS NULL`) != 0 {
		t.Fatal("the old link left the account's own grant open")
	}
}

// addressSource stands for Keycloak with core's row: the person's addresses
// include a Personal e-mail core does not hold.
type addressSource map[uuid.UUID][]string

func (a addressSource) ErasureAddresses(_ context.Context, id uuid.UUID) ([]string, error) {
	if addresses, ok := a[id]; ok {
		return addresses, nil
	}
	return nil, errors.New("identity: user address lookup failed with status 503")
}

func TestMineAndWithdrawMineCoverTheIdentitysPersonalAddress(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	member := uuid.New()
	f.exec(t, `INSERT INTO users (id, email) VALUES ($1, 'm@example.com')`, member)
	f.svc.SetAddresses(addressSource{member: {"m@example.com", "Personal@Example.org"}})
	if _, err := f.svc.Grant(ctx, consent.Grant{Purpose: consent.PurposeEventInvitations, Email: "personal@example.org",
		Source: consent.SourceGuessr, ClientID: "guessr", EmailVerified: true}); err != nil {
		t.Fatal(err)
	}
	own, err := f.svc.Mine(ctx, member)
	if err != nil || len(own) != 1 || own[0].Subject != "address" {
		t.Fatalf("mine %+v %v", own, err)
	}
	if n, err := f.svc.WithdrawMine(ctx, member, consent.PurposeEventInvitations); err != nil || n != 1 {
		t.Fatalf("withdraw mine %d %v", n, err)
	}
	// Keycloak unreachable: the person's list is refused rather than shown
	// short, and nothing is withdrawn half-way.
	stranger := uuid.New()
	f.exec(t, `INSERT INTO users (id, email) VALUES ($1, 's@example.com')`, stranger)
	if _, err := f.svc.Mine(ctx, stranger); err == nil || strings.Contains(err.Error(), "s@example.com") {
		t.Fatalf("mine without addresses: %v", err)
	}
	if _, err := f.svc.WithdrawMine(ctx, stranger, consent.PurposeEventInvitations); err == nil {
		t.Fatal("withdraw mine without addresses went on")
	}
}

// Lookup at a club's scale (3,000 accounts, 3,000 address grants, 300
// members' own grants, 20,000 guest Tickets with check-ins) answers 500
// addresses well within a request's time, and so does an audience page.
func TestLookupAndAudienceStayFastAtClubScale(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.exec(t, `INSERT INTO users (id, email, school_email)
		SELECT gen_random_uuid(), 'u' || g || '@example.com', 'u' || g || '@std.yildiz.edu.tr' FROM generate_series(1, 3000) g`)
	emails := make([]string, 3000)
	hmacs := make([][]byte, 3000)
	for i := range emails {
		emails[i] = fmt.Sprintf("g%d@example.com", i)
		hmacs[i] = f.svc.EmailHMAC(emails[i])
	}
	f.exec(t, `INSERT INTO contact_consents (id, purpose, email, email_hmac, source, client_id, text_version, granted_at, confirmed_at, confirmed_via)
		SELECT gen_random_uuid(), 'event_invitations', e, h, 'place', 'place', 'davet-v1', $3, $3, 'service'
		FROM unnest($1::text[], $2::bytea[]) AS a(e, h)`, emails, hmacs, f.now)
	f.exec(t, `INSERT INTO contact_consents (id, purpose, user_id, source, text_version, granted_at, confirmed_at, confirmed_via)
		SELECT gen_random_uuid(), 'event_invitations', id, 'self', 'davet-v1', $1, $1, 'account' FROM users
		WHERE split_part(substr(email, 2), '@', 1)::int <= 300`, f.now)
	f.exec(t, `INSERT INTO events (id, name, location, owner_team) SELECT gen_random_uuid(), 'E' || g, 'YTÜ', 'WEBLAB' FROM generate_series(1, 40) g`)
	f.exec(t, `INSERT INTO tickets (id, event_id, ticket_type, guest_first_name, guest_last_name, guest_email)
		SELECT gen_random_uuid(), e.id, 'GUEST', 'A', 'B', 'g' || (row_number() OVER ()) || '@example.com'
		FROM events e, generate_series(1, 500) g`)
	f.exec(t, `INSERT INTO event_days (id, event_id) SELECT gen_random_uuid(), id FROM events`)
	f.exec(t, `INSERT INTO sessions (id, event_day_id, title, session_type) SELECT gen_random_uuid(), id, 'S', 'PRESENTATION' FROM event_days`)
	f.exec(t, `INSERT INTO ticket_checkins (id, ticket_id, event_day_id, session_id, created_at)
		SELECT gen_random_uuid(), t.id, d.id, s.id, $1::timestamptz - interval '1 day'
		FROM tickets t JOIN event_days d ON d.event_id = t.event_id JOIN sessions s ON s.event_day_id = d.id`, f.now)
	f.exec(t, `ANALYZE`)

	asked := make([]string, 0, 500)
	for i := range 495 {
		asked = append(asked, emails[i*6])
	}
	asked = append(asked, "u1@example.com", "U2@std.yildiz.edu.tr", "u2999@example.com", "nobody@example.com", "u1@example.com")
	for _, n := range []int{5, 50, 500} {
		start := time.Now()
		states, err := f.svc.Lookup(ctx, consent.PurposeEventInvitations, asked[len(asked)-n:])
		took := time.Since(start)
		t.Logf("lookup of %d addresses: %d states in %s", n, len(states), took)
		if err != nil {
			t.Fatal(err)
		}
		if took > 3*time.Second {
			t.Fatalf("lookup of %d addresses took %s", n, took)
		}
	}
	states, err := f.svc.Lookup(ctx, consent.PurposeEventInvitations, asked)
	if err != nil {
		t.Fatal(err)
	}
	// 495 address grants; u1 and u2 hold their own grant (the first 300 by
	// address), u2999 does not; nobody has none.
	if len(states) != 497 || states["u1@example.com"] != consent.StatusActive || states["u2@std.yildiz.edu.tr"] != consent.StatusActive ||
		states["u2999@example.com"] != "" || states["nobody@example.com"] != "" {
		t.Fatalf("%d states: u1=%q u2=%q u2999=%q", len(states), states["u1@example.com"], states["u2@std.yildiz.edu.tr"], states["u2999@example.com"])
	}

	start := time.Now()
	entries, _, err := f.svc.Audience(ctx, consent.PurposeEventInvitations, uuid.Nil, consent.MaxAudiencePage)
	took := time.Since(start)
	t.Logf("audience page of %d in %s", len(entries), took)
	if err != nil || len(entries) != consent.MaxAudiencePage {
		t.Fatalf("audience %d %v", len(entries), err)
	}
	if took > 5*time.Second {
		t.Fatalf("an audience page took %s", took)
	}

	// The caller's end stops the query.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := f.svc.Lookup(cancelled, consent.PurposeEventInvitations, asked); !errors.Is(err, context.Canceled) {
		t.Fatalf("lookup with an ended context: %v", err)
	}
}
