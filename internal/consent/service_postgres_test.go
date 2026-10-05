package consent_test

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/consent"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
)

var testKey = bytes.Repeat([]byte{7}, 32)

type sentMail struct {
	template, recipient string
	vars                map[string]string
}

type fakeMailer struct {
	mu   sync.Mutex
	sent []sentMail
}

func (m *fakeMailer) ConsentConfirmation(_ context.Context, template, recipient string, vars map[string]string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, sentMail{template, recipient, vars})
}

func (m *fakeMailer) all() []sentMail {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]sentMail(nil), m.sent...)
}

type fixture struct {
	pool *pgxpool.Pool
	svc  *consent.Service
	mail *fakeMailer
	now  time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pool := testpostgres.Start(t)
	if err := migrate.Apply(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	f := &fixture{pool: pool, mail: &fakeMailer{}, now: time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)}
	// Place, Guessr and Forms may assert a verified address here; in
	// production nobody may until CONTACT_CONSENT_VERIFIED_CLIENTS names them.
	f.svc = consent.NewService(pool, consent.TestConfig(testKey, "https://api.example.test/", "place", "guessr", "forms"), f.mail)
	f.svc.SetClock(func() time.Time { return f.now })
	f.svc.SetAsync(func(fn func()) { fn() })
	return f
}

func (f *fixture) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func (f *fixture) count(t *testing.T, where string, args ...any) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM contact_consents WHERE `+where, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func tokenOf(t *testing.T, link string) string {
	t.Helper()
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query().Get("token")
}

func TestGuestGrantWaitsForItsConfirmationAndKeepsNoIP(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	eventID := uuid.New()
	f.exec(t, `INSERT INTO events (id, name, location, owner_team) VALUES ($1, 'Hack', 'YTÜ', 'WEBLAB')`, eventID)

	result, err := f.svc.Grant(ctx, consent.Grant{
		Purpose: consent.PurposeEventInvitations, Email: "  Ada@Example.COM ", Source: consent.SourceGuestApply,
		EventID: &eventID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != consent.StatusPending || !result.Created {
		t.Fatalf("result %+v", result)
	}
	mails := f.mail.all()
	if len(mails) != 1 || mails[0].recipient != "ada@example.com" || mails[0].template != consent.DefaultConfirmTemplateKey {
		t.Fatalf("mails %+v", mails)
	}
	if !strings.HasPrefix(mails[0].vars["confirmUrl"], "https://api.example.test/v1/consents/confirm?token=") ||
		!strings.HasPrefix(mails[0].vars["withdrawUrl"], "https://api.example.test/v1/consents/withdraw?token=") {
		t.Fatalf("links %v", mails[0].vars)
	}
	for _, link := range mails[0].vars {
		if strings.Contains(link, "ada") {
			t.Fatalf("a link carries the address: %s", link)
		}
	}
	if f.count(t, `email = 'ada@example.com' AND text_version = 'davet-v1' AND event_id = $1 AND confirmed_at IS NULL`, eventID) != 1 {
		t.Fatal("pending row not stored as expected")
	}

	// A pending grant is not in the audience.
	entries, _, err := f.svc.Audience(ctx, consent.PurposeEventInvitations, uuid.Nil, 10)
	if err != nil || len(entries) != 0 {
		t.Fatalf("audience %v %v", entries, err)
	}

	// The same grant again within a day sends no second mail (the cap is
	// TestConfirmationMailIsCappedAtOneADayAndThreeInAll).
	if _, err := f.svc.Grant(ctx, consent.Grant{Purpose: consent.PurposeEventInvitations, Email: "ada@example.com", Source: consent.SourceGuestApply}); err != nil {
		t.Fatal(err)
	}
	if len(f.mail.all()) != 1 || f.count(t, `true`) != 1 {
		t.Fatal("a repeated grant mailed again or wrote a second row")
	}

	outcome, err := f.svc.Confirm(ctx, tokenOf(t, mails[0].vars["confirmUrl"]))
	if err != nil || outcome != consent.OutcomeConfirmed {
		t.Fatalf("confirm %v %v", outcome, err)
	}
	entries, next, err := f.svc.Audience(ctx, consent.PurposeEventInvitations, uuid.Nil, 10)
	if err != nil || next != uuid.Nil || len(entries) != 1 || entries[0].Email != "ada@example.com" || entries[0].Subject != "address" || entries[0].RenewalDue {
		t.Fatalf("audience %+v %v %v", entries, next, err)
	}
	if f.count(t, `confirmed_via = 'link'`) != 1 {
		t.Fatal("confirmation not recorded as by link")
	}

	// The table has no column for a client address or user agent.
	var columns int
	if err := f.pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns
		WHERE table_name = 'contact_consents' AND (column_name LIKE '%ip%' OR column_name LIKE '%agent%' OR column_name LIKE '%name%')`).Scan(&columns); err != nil {
		t.Fatal(err)
	}
	if columns != 0 {
		t.Fatalf("contact_consents has %d address, agent or name columns", columns)
	}
}

func TestConfirmLinkExpiresAndAVerifiedGrantIsActiveAtOnce(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.svc.Grant(ctx, consent.Grant{Purpose: consent.PurposeEventInvitations, Email: "a@example.com", Source: consent.SourceGuestApply}); err != nil {
		t.Fatal(err)
	}
	link := tokenOf(t, f.mail.all()[0].vars["confirmUrl"])
	f.now = f.now.Add(consent.PendingTTL + time.Minute)
	if _, err := f.svc.Confirm(ctx, link); !errors.Is(err, consent.ErrLinkExpired) {
		t.Fatalf("expired confirm link: %v", err)
	}
	if _, err := f.svc.ConfirmLinkGrant(ctx, link); !errors.Is(err, consent.ErrLinkExpired) {
		t.Fatalf("check expired: %v", err)
	}

	// Place verified the address: active, no mail.
	result, err := f.svc.Grant(ctx, consent.Grant{
		Purpose: consent.PurposeEventInvitations, Email: "b@example.com", Source: consent.SourcePlace, ClientID: "place", EmailVerified: true,
	})
	if err != nil || result.Status != consent.StatusActive {
		t.Fatalf("verified grant %+v %v", result, err)
	}
	if len(f.mail.all()) != 1 {
		t.Fatal("a verified grant was mailed")
	}
	// A verified grant replaces a pending one with a row of its own
	// (TestVerifiedGrantSupersedesAPendingOneWithItsOwnEvidence).
	result, err = f.svc.Grant(ctx, consent.Grant{Purpose: consent.PurposeEventInvitations, Email: "a@example.com", Source: consent.SourceForms, ClientID: "forms", EmailVerified: true})
	if err != nil || result.Status != consent.StatusActive || !result.Created {
		t.Fatalf("verified over pending %+v %v", result, err)
	}
	if f.count(t, `confirmed_via = 'service'`) != 2 {
		t.Fatal("service confirmations not recorded")
	}
}

func TestGrantRefusesWhatItCannotProve(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for name, g := range map[string]consent.Grant{
		"unknown purpose":      {Purpose: "newsletter", Email: "a@example.com", Source: consent.SourceGuestApply},
		"no address":           {Purpose: consent.PurposeEventInvitations, Email: "not-an-address", Source: consent.SourceGuestApply},
		"pool through apply":   {Purpose: consent.PurposeRecruitmentPool, Email: "a@example.com", Source: consent.SourceGuestApply},
		"pool through place":   {Purpose: consent.PurposeRecruitmentPool, Email: "a@example.com", Source: consent.SourcePlace},
		"account and address":  {Purpose: consent.PurposeEventInvitations, UserID: uuid.New(), Email: "a@example.com", Source: consent.SourceSelf},
		"unknown text version": {Purpose: consent.PurposeEventInvitations, TextVersion: "davet-v9", Email: "a@example.com", Source: consent.SourceGuestApply},
	} {
		if _, err := f.svc.Grant(ctx, g); !errors.Is(err, consent.ErrInvalid) && !errors.Is(err, consent.ErrUnknownText) &&
			!errors.Is(err, consent.ErrPurposeNotEnabled) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if f.count(t, `true`) != 0 {
		t.Fatal("a refused grant was stored")
	}
}

// The recruitment pool is not taken yet, not even from Forms, and nothing
// reads or withdraws it.
func TestRecruitmentPoolIsNotEnabled(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	pool := consent.PurposeRecruitmentPool
	if _, err := f.svc.Grant(ctx, consent.Grant{Purpose: pool, TextVersion: "alim-havuzu-v1", Email: "a@example.com",
		Source: consent.SourceForms, ClientID: "forms", EmailVerified: true}); !errors.Is(err, consent.ErrPurposeNotEnabled) {
		t.Fatalf("grant: %v", err)
	}
	if _, err := f.svc.Lookup(ctx, pool, []string{"a@example.com"}); !errors.Is(err, consent.ErrPurposeNotEnabled) {
		t.Fatalf("lookup: %v", err)
	}
	if _, err := f.svc.WithdrawForAddress(ctx, pool, "a@example.com"); !errors.Is(err, consent.ErrPurposeNotEnabled) {
		t.Fatalf("withdraw: %v", err)
	}
	if _, _, err := f.svc.Audience(ctx, pool, uuid.Nil, 10); !errors.Is(err, consent.ErrPurposeNotEnabled) {
		t.Fatalf("audience: %v", err)
	}
	if f.count(t, `true`) != 0 {
		t.Fatal("a recruitment pool grant was stored")
	}
}

func TestWithdrawLinkEndsTheSubjectsCurrentGrantAndCanBeRepeated(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	grant := consent.Grant{Purpose: consent.PurposeEventInvitations, Email: "a@example.com", Source: consent.SourcePlace, ClientID: "place", EmailVerified: true}
	first, err := f.svc.Grant(ctx, grant)
	if err != nil {
		t.Fatal(err)
	}
	entries, _, err := f.svc.Audience(ctx, consent.PurposeEventInvitations, uuid.Nil, 10)
	if err != nil || len(entries) != 1 {
		t.Fatalf("audience %v %v", entries, err)
	}
	withdraw := tokenOf(t, entries[0].WithdrawURL)
	if grant, err := f.svc.WithdrawLinkGrant(ctx, withdraw); err != nil || grant.Purpose != consent.PurposeEventInvitations ||
		grant.TextVersion != "davet-v1" || grant.Status != consent.StatusActive {
		t.Fatalf("withdraw link grant %+v %v", grant, err)
	}
	if _, err := f.svc.WithdrawLinkGrant(ctx, withdraw[:len(withdraw)-2]+"AA"); !errors.Is(err, consent.ErrLink) {
		t.Fatalf("tampered link: %v", err)
	}

	outcome, err := f.svc.Withdraw(ctx, withdraw, consent.WithdrawByOneClick)
	if err != nil || outcome != consent.OutcomeWithdrawn {
		t.Fatalf("withdraw %v %v", outcome, err)
	}
	outcome, err = f.svc.Withdraw(ctx, withdraw, consent.WithdrawByPage)
	if err != nil || outcome != consent.OutcomeNothingOpen {
		t.Fatalf("withdraw again %v %v", outcome, err)
	}
	if f.count(t, `id = $1 AND ended_reason = 'withdrawn' AND ended_via = 'one_click' AND email IS NULL AND email_hmac IS NOT NULL`, first.ID) != 1 {
		t.Fatal("withdrawn grant keeps its address or lost its proof key")
	}

	// A new grant is a new row; the old invitation's link withdraws it too.
	second, err := f.svc.Grant(ctx, grant)
	if err != nil || second.ID == first.ID || second.Status != consent.StatusActive {
		t.Fatalf("grant again %+v %v", second, err)
	}
	if outcome, err := f.svc.Withdraw(ctx, withdraw, consent.WithdrawByPage); err != nil || outcome != consent.OutcomeWithdrawn {
		t.Fatalf("old link on the new grant %v %v", outcome, err)
	}
	// A confirm link of a grant withdrawn before it was confirmed does not
	// reopen it.
	if _, err := f.svc.Grant(ctx, consent.Grant{Purpose: consent.PurposeEventInvitations, Email: "c@example.com", Source: consent.SourceGuestApply}); err != nil {
		t.Fatal(err)
	}
	mail := f.mail.all()[0]
	if outcome, err := f.svc.Withdraw(ctx, tokenOf(t, mail.vars["withdrawUrl"]), consent.WithdrawByPage); err != nil || outcome != consent.OutcomeWithdrawn {
		t.Fatalf("withdraw pending %v %v", outcome, err)
	}
	if outcome, err := f.svc.Confirm(ctx, tokenOf(t, mail.vars["confirmUrl"])); err != nil || outcome != consent.OutcomeEnded {
		t.Fatalf("confirm after withdrawal %v %v", outcome, err)
	}
	if f.count(t, `ended_at IS NULL AND purpose = 'event_invitations'`) != 0 {
		t.Fatal("open invitation grants left")
	}
}

func TestRenewalIsDueAfterThreeYearsWithoutAttendance(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	eventID, dayID, sessionID := uuid.New(), uuid.New(), uuid.New()
	f.exec(t, `INSERT INTO events (id, name, location, owner_team) VALUES ($1, 'Hack', 'YTÜ', 'WEBLAB')`, eventID)
	f.exec(t, `INSERT INTO event_days (id, event_id) VALUES ($1, $2)`, dayID, eventID)
	f.exec(t, `INSERT INTO sessions (id, event_day_id, title, session_type) VALUES ($1, $2, 'Oturum', 'PRESENTATION')`, sessionID, dayID)

	for _, email := range []string{"idle@example.com", "came@example.com"} {
		if _, err := f.svc.Grant(ctx, consent.Grant{Purpose: consent.PurposeEventInvitations, Email: email, Source: consent.SourcePlace, ClientID: "place", EmailVerified: true}); err != nil {
			t.Fatal(err)
		}
	}
	ticketID := uuid.New()
	f.exec(t, `INSERT INTO tickets (id, event_id, ticket_type, guest_first_name, guest_last_name, guest_email) VALUES ($1, $2, 'GUEST', 'C', 'D', 'came@example.com')`, ticketID, eventID)
	f.exec(t, `INSERT INTO ticket_checkins (id, ticket_id, event_day_id, session_id, created_at) VALUES ($1, $2, $3, $4, $5)`,
		uuid.New(), ticketID, dayID, sessionID, f.now.Add(2*365*24*time.Hour))

	f.now = f.now.Add(consent.RenewalAfter + 24*time.Hour)
	entries, _, err := f.svc.Audience(ctx, consent.PurposeEventInvitations, uuid.Nil, 10)
	if err != nil || len(entries) != 2 {
		t.Fatalf("audience %v %v", entries, err)
	}
	due := map[string]consent.AudienceEntry{}
	for _, entry := range entries {
		due[entry.Email] = entry
	}
	if !due["idle@example.com"].RenewalDue || due["idle@example.com"].RenewURL == "" {
		t.Fatalf("idle grant not due: %+v", due["idle@example.com"])
	}
	if due["came@example.com"].RenewalDue || due["came@example.com"].RenewURL != "" {
		t.Fatalf("attended grant due: %+v", due["came@example.com"])
	}

	asked, err := f.svc.RequestRenewal(ctx, consent.PurposeEventInvitations, []uuid.UUID{due["idle@example.com"].ID, due["came@example.com"].ID})
	if err != nil || asked != 1 {
		t.Fatalf("renewal requests %d %v", asked, err)
	}
	outcome, err := f.svc.Confirm(ctx, tokenOf(t, due["idle@example.com"].RenewURL))
	if err != nil || outcome != consent.OutcomeRenewed {
		t.Fatalf("renew %v %v", outcome, err)
	}
	entries, _, _ = f.svc.Audience(ctx, consent.PurposeEventInvitations, uuid.Nil, 10)
	for _, entry := range entries {
		if entry.RenewalDue || entry.RenewalRequestedAt != nil {
			t.Fatalf("renewed grant still due: %+v", entry)
		}
	}
}

func TestAudiencePagesAndReadsAMembersCurrentAddress(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	member := uuid.New()
	f.exec(t, `INSERT INTO users (id, email) VALUES ($1, 'member@example.com')`, member)
	result, err := f.svc.Grant(ctx, consent.Grant{Purpose: consent.PurposeEventInvitations, UserID: member, Source: consent.SourceSelf, ClientID: "account-center"})
	if err != nil || result.Status != consent.StatusActive {
		t.Fatalf("self grant %+v %v", result, err)
	}
	for i := range 4 {
		email := string(rune('a'+i)) + "@example.com"
		if _, err := f.svc.Grant(ctx, consent.Grant{Purpose: consent.PurposeEventInvitations, Email: email, Source: consent.SourcePlace, ClientID: "place", EmailVerified: true}); err != nil {
			t.Fatal(err)
		}
	}
	f.exec(t, `UPDATE users SET email = 'renamed@example.com' WHERE id = $1`, member)

	var all []consent.AudienceEntry
	after := uuid.Nil
	for pages := 0; ; pages++ {
		entries, next, err := f.svc.Audience(ctx, consent.PurposeEventInvitations, after, 2)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, entries...)
		if next == uuid.Nil {
			break
		}
		if pages > 5 {
			t.Fatal("paging does not end")
		}
		after = next
	}
	if len(all) != 5 {
		t.Fatalf("audience %d entries", len(all))
	}
	found := false
	for _, entry := range all {
		if entry.Subject == "account" {
			found = entry.Email == "renamed@example.com"
		}
	}
	if !found {
		t.Fatal("a member's grant does not follow their address")
	}
}

func TestPersonListsAndWithdrawsTheirOwnGrants(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	member := uuid.New()
	f.exec(t, `INSERT INTO users (id, email, school_email) VALUES ($1, 'm@example.com', 'm@std.yildiz.edu.tr')`, member)
	if _, err := f.svc.Grant(ctx, consent.Grant{Purpose: consent.PurposeEventInvitations, UserID: member, Source: consent.SourceSelf}); err != nil {
		t.Fatal(err)
	}
	// Given as a guest with their school address before they signed up.
	if _, err := f.svc.Grant(ctx, consent.Grant{Purpose: consent.PurposeEventInvitations, Email: "M@std.yildiz.edu.tr", Source: consent.SourceGuestApply}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Grant(ctx, consent.Grant{Purpose: consent.PurposeEventInvitations, Email: "stranger@example.com", Source: consent.SourceGuestApply}); err != nil {
		t.Fatal(err)
	}
	own, err := f.svc.Mine(ctx, member)
	if err != nil || len(own) != 2 {
		t.Fatalf("mine %+v %v", own, err)
	}
	n, err := f.svc.WithdrawMine(ctx, member, consent.PurposeEventInvitations)
	if err != nil || n != 2 {
		t.Fatalf("withdraw mine %d %v", n, err)
	}
	own, _ = f.svc.Mine(ctx, member)
	for _, grant := range own {
		if grant.Status != consent.StatusWithdrawn {
			t.Fatalf("own grant %+v", grant)
		}
	}
	if f.count(t, `ended_at IS NULL`) != 1 {
		t.Fatal("someone else's grant was withdrawn")
	}
}

func TestServiceLookupAndWithdrawByAddress(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.svc.Grant(ctx, consent.Grant{Purpose: consent.PurposeEventInvitations, Email: "kept@example.com", Source: consent.SourceForms, ClientID: "forms", EmailVerified: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Grant(ctx, consent.Grant{Purpose: consent.PurposeEventInvitations, Email: "waiting@example.com", Source: consent.SourceForms}); err != nil {
		t.Fatal(err)
	}
	states, err := f.svc.Lookup(ctx, consent.PurposeEventInvitations, []string{"KEPT@example.com", "waiting@example.com", "none@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if states["kept@example.com"] != consent.StatusActive || states["waiting@example.com"] != consent.StatusPending || len(states) != 2 {
		t.Fatalf("states %v", states)
	}
	ended, err := f.svc.WithdrawForAddress(ctx, consent.PurposeEventInvitations, "kept@example.com")
	if err != nil || !ended {
		t.Fatalf("withdraw for address %v %v", ended, err)
	}
	if ended, err := f.svc.WithdrawForAddress(ctx, consent.PurposeEventInvitations, "kept@example.com"); err != nil || ended {
		t.Fatalf("withdraw again %v %v", ended, err)
	}
	if f.count(t, `ended_via = 'service' AND email IS NULL`) != 1 {
		t.Fatal("service withdrawal not recorded")
	}
}

func TestEraseSubjectDeletesEveryGrantOfThePerson(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	member := uuid.New()
	f.exec(t, `INSERT INTO users (id, email) VALUES ($1, 'm@example.com')`, member)
	if _, err := f.svc.Grant(ctx, consent.Grant{Purpose: consent.PurposeEventInvitations, UserID: member, Source: consent.SourceSelf}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Grant(ctx, consent.Grant{Purpose: consent.PurposeEventInvitations, Email: "m@example.com", Source: consent.SourceForms, ClientID: "forms", EmailVerified: true}); err != nil {
		t.Fatal(err)
	}
	// An ended grant keeps only its proof key; it goes too.
	if _, err := f.svc.Grant(ctx, consent.Grant{Purpose: consent.PurposeEventInvitations, Email: "old@example.com", Source: consent.SourcePlace, ClientID: "place", EmailVerified: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.WithdrawForAddress(ctx, consent.PurposeEventInvitations, "old@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Grant(ctx, consent.Grant{Purpose: consent.PurposeEventInvitations, Email: "other@example.com", Source: consent.SourcePlace, ClientID: "place", EmailVerified: true}); err != nil {
		t.Fatal(err)
	}
	n, err := f.svc.EraseSubject(ctx, member, []string{"M@example.com", "old@example.com"})
	if err != nil || n != 3 {
		t.Fatalf("erased %d %v", n, err)
	}
	if f.count(t, `true`) != 1 {
		t.Fatal("another person's grant was erased")
	}
	// Repeating it is harmless.
	if n, err := f.svc.EraseSubject(ctx, member, []string{"m@example.com"}); err != nil || n != 0 {
		t.Fatalf("erase again %d %v", n, err)
	}
}

func TestPersonGrantNeedsAnActiveAccount(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	member := uuid.New()
	f.exec(t, `INSERT INTO users (id, email, account_state) VALUES ($1, 'gone@example.com', 'anonymized')`, member)
	if _, err := f.svc.Grant(ctx, consent.Grant{Purpose: consent.PurposeEventInvitations, UserID: member, Source: consent.SourceSelf}); err == nil {
		t.Fatal("a grant named an account that is not active")
	}
}

// A person's own grant and a grant given for their address both send to the
// same mailbox. Withdrawing from either's link, from the product, or from the
// account ends both: one click ends the mails, whichever grant the mail was
// sent on.
func TestWithdrawingEndsEveryGrantThatMailsTheSameAddress(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	member, other := uuid.New(), uuid.New()
	f.exec(t, `INSERT INTO users (id, email, school_email) VALUES ($1, 'm@example.com', 'm@std.yildiz.edu.tr')`, member)
	f.exec(t, `INSERT INTO users (id, email) VALUES ($1, 'other@example.com')`, other)
	grantBoth := func() {
		t.Helper()
		for _, g := range []consent.Grant{
			{Purpose: consent.PurposeEventInvitations, UserID: member, Source: consent.SourceSelf},
			{Purpose: consent.PurposeEventInvitations, Email: "M@example.com", Source: consent.SourcePlace, ClientID: "place", EmailVerified: true},
			{Purpose: consent.PurposeEventInvitations, Email: "m@std.yildiz.edu.tr", Source: consent.SourceGuessr, ClientID: "guessr", EmailVerified: true},
			{Purpose: consent.PurposeEventInvitations, UserID: other, Source: consent.SourceSelf},
		} {
			if _, err := f.svc.Grant(ctx, g); err != nil {
				t.Fatal(err)
			}
		}
	}
	open := func() int { return f.count(t, `ended_at IS NULL`) }
	entryOf := func(subject, email string) consent.AudienceEntry {
		t.Helper()
		entries, _, err := f.svc.Audience(ctx, consent.PurposeEventInvitations, uuid.Nil, 100)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if entry.Subject == subject && entry.Email == email {
				return entry
			}
		}
		t.Fatalf("no %s entry for %s in %+v", subject, email, entries)
		return consent.AudienceEntry{}
	}

	grantBoth()
	if outcome, err := f.svc.Withdraw(ctx, tokenOf(t, entryOf("account", "m@example.com").WithdrawURL), consent.WithdrawByOneClick); err != nil || outcome != consent.OutcomeWithdrawn {
		t.Fatalf("account link %v %v", outcome, err)
	}
	if open() != 1 || f.count(t, `ended_at IS NULL AND user_id = $1`, other) != 1 {
		t.Fatal("the account's link left a grant that mails the same person open, or ended someone else's")
	}

	grantBoth()
	if outcome, err := f.svc.Withdraw(ctx, tokenOf(t, entryOf("address", "m@example.com").WithdrawURL), consent.WithdrawByPage); err != nil || outcome != consent.OutcomeWithdrawn {
		t.Fatalf("address link %v %v", outcome, err)
	}
	if f.count(t, `ended_at IS NULL AND (user_id = $1 OR email = 'm@example.com')`, member) != 0 {
		t.Fatal("the address's link left the account's grant open")
	}
	// The school address mails another mailbox; its grant is the person's
	// too, but its own link withdraws it.
	if f.count(t, `ended_at IS NULL AND email = 'm@std.yildiz.edu.tr'`) != 1 {
		t.Fatal("the address link ended a grant of another address")
	}
	if _, err := f.svc.Withdraw(ctx, tokenOf(t, entryOf("address", "m@std.yildiz.edu.tr").WithdrawURL), consent.WithdrawByPage); err != nil {
		t.Fatal(err)
	}

	grantBoth()
	if ended, err := f.svc.WithdrawForAddress(ctx, consent.PurposeEventInvitations, "m@example.com"); err != nil || !ended {
		t.Fatalf("withdraw for address %v %v", ended, err)
	}
	if f.count(t, `ended_at IS NULL AND (user_id = $1 OR email = 'm@example.com')`, member) != 0 {
		t.Fatal("a product's withdrawal left the account's grant open")
	}
	if _, err := f.svc.WithdrawMine(ctx, member, consent.PurposeEventInvitations); err != nil {
		t.Fatal(err)
	}
	if open() != 1 {
		t.Fatalf("open grants after the person withdrew everything: %d", open())
	}
}

// Lookup answers for the address, whichever grant mails it: Place keeps a
// player's address while any grant for it is open.
func TestLookupCountsTheAccountsOwnGrantForItsAddress(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	member := uuid.New()
	f.exec(t, `INSERT INTO users (id, email) VALUES ($1, 'm@example.com')`, member)
	if _, err := f.svc.Grant(ctx, consent.Grant{Purpose: consent.PurposeEventInvitations, UserID: member, Source: consent.SourceSelf}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Grant(ctx, consent.Grant{Purpose: consent.PurposeEventInvitations, Email: "pending@example.com", Source: consent.SourceGuestApply}); err != nil {
		t.Fatal(err)
	}
	states, err := f.svc.Lookup(ctx, consent.PurposeEventInvitations, []string{"M@Example.com", "pending@example.com", "none@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if states["m@example.com"] != consent.StatusActive || states["pending@example.com"] != consent.StatusPending || len(states) != 2 {
		t.Fatalf("states %v", states)
	}
	// An account on its way to erasure no longer counts.
	f.exec(t, `UPDATE users SET account_state = 'deletion_pending' WHERE id = $1`, member)
	if states, err := f.svc.Lookup(ctx, consent.PurposeEventInvitations, []string{"m@example.com"}); err != nil || len(states) != 0 {
		t.Fatalf("states of an account being erased %v %v", states, err)
	}
}
