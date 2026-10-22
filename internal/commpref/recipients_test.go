package commpref

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/identity"
)

const testYTUAlias = "OBS"

// fakeAddressBook is the Keycloak of the recipient tests: the realm's
// accounts in the order Keycloak pages them, the accounts linked to the
// YTÜ identity provider, and counters of every page read.
type fakeAddressBook struct {
	mu        sync.Mutex
	holders   []identity.AddressHolder
	linked    []uuid.UUID
	pageReads int
	linkReads int
	// failAt fails the holders page read starting at this offset (-1:
	// none); failLinks fails every link read.
	failAt    int
	failLinks bool
	// repeat answers the first row of every page after the first again in
	// front of it, as offset paging does when a user is added while the
	// realm is read.
	repeat bool
	// gate, when set, holds every link read until it is closed; started
	// hears of each read that reached it.
	gate    chan struct{}
	started chan struct{}
}

var errKeycloakDown = errors.New("keycloak: status 502")

func (b *fakeAddressBook) AddressHoldersPage(_ context.Context, first, max int) (identity.AddressHolderPage, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pageReads++
	if b.failAt >= 0 && first >= b.failAt {
		return identity.AddressHolderPage{}, errKeycloakDown
	}
	start := min(first, len(b.holders))
	if b.repeat && start > 0 {
		start--
	}
	rows := slices.Clone(b.holders[start:min(start+max, len(b.holders))])
	return identity.AddressHolderPage{Holders: rows, Rows: len(rows)}, nil
}

func (b *fakeAddressBook) IdentityProviderLinkedPage(_ context.Context, alias string, first, max int) (identity.LinkedPage, error) {
	b.mu.Lock()
	gate, started := b.gate, b.started
	b.mu.Unlock()
	if started != nil {
		started <- struct{}{}
	}
	if gate != nil {
		<-gate
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.linkReads++
	if b.failLinks {
		return identity.LinkedPage{}, errKeycloakDown
	}
	if alias != testYTUAlias {
		return identity.LinkedPage{}, nil
	}
	start := min(first, len(b.linked))
	ids := slices.Clone(b.linked[start:min(start+max, len(b.linked))])
	return identity.LinkedPage{IDs: ids, Rows: len(ids)}, nil
}

func (b *fakeAddressBook) reads() (pages, links int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.pageReads, b.linkReads
}

func (b *fakeAddressBook) set(change func(*fakeAddressBook)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	change(b)
}

type recipientsFixture struct {
	t          *testing.T
	kc         *fakeKeycloak
	book       *fakeAddressBook
	accounts   fakeAccounts
	store      *MemoryStore
	prefs      *Service
	recipients *Recipients
	clockMu    sync.Mutex
	clock      time.Time
	p          map[string]uuid.UUID
}

// newRecipientsFixture is the test tree (testGroups) and these accounts,
// each with the Primary e-mail <name>@example.test, verified and enabled,
// unless said:
//
//	member: in WEBLAB (no Group is read by the question: membership plays
//	  no part)
//	guest: in no Group
//	unverified: Primary e-mail not verified
//	ytu: School e-mail Okul.Adi@STD.Yildiz.edu.tr  , linked to YTÜ, never
//	  signed in to core
//	unlinked: School e-mail unlinked@std.yildiz.edu.tr, not linked
//	personal: Personal e-mail kisisel@example.org, stamped
//	unstamped: Personal e-mail damgasiz@example.org, no stamp
//	badStamp: Personal e-mail bozuk@example.org, a stamp that reads as no
//	  moment
//	twinA, twinB: twinA's Primary is ortak@example.test, twinB's Personal
//	  (stamped) too
//	disabled: disabled in Keycloak
//	erased: being erased in core
//	service: a client's service account, Primary service@example.test
func newRecipientsFixture(t *testing.T) *recipientsFixture {
	t.Helper()
	f := &recipientsFixture{
		t:        t,
		kc:       &fakeKeycloak{groups: testGroups(), members: map[string][]uuid.UUID{}, people: map[uuid.UUID]identity.AudiencePerson{}},
		book:     &fakeAddressBook{failAt: -1},
		accounts: fakeAccounts{blocked: map[uuid.UUID]bool{}, names: map[uuid.UUID][2]string{}},
		store:    NewMemoryStore(),
		clock:    time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC),
		p:        map[string]uuid.UUID{},
	}
	f.prefs = NewService(TestConfig(testKey(), "https://api.example.test"), f.store, f.kc, authz.DefaultPrivilegedRoots())
	f.recipients = NewRecipients(f.prefs, f.book, f.accounts, testYTUAlias)
	f.prefs.SetClock(f.now)
	f.recipients.SetClock(f.now)
	f.recipients.SetPageSize(3)
	for _, name := range []string{"member", "guest", "unverified", "ytu", "unlinked", "personal", "unstamped", "badStamp", "twinA", "twinB", "disabled", "erased", "service"} {
		f.add(name, nil)
	}
	f.kc.members["g-weblab"] = []uuid.UUID{f.p["member"]}
	f.edit("unverified", func(h *identity.AddressHolder) { h.EmailVerified = false })
	f.edit("ytu", func(h *identity.AddressHolder) { h.SchoolEmail = " Okul.Adi@STD.Yildiz.edu.tr  " })
	f.book.linked = []uuid.UUID{f.p["ytu"]}
	f.edit("unlinked", func(h *identity.AddressHolder) { h.SchoolEmail = "unlinked@std.yildiz.edu.tr" })
	f.edit("personal", func(h *identity.AddressHolder) {
		h.PersonalEmail, h.PersonalEmailVerifiedAt = "kisisel@example.org", "2026-09-21T14:13:20Z"
	})
	f.edit("unstamped", func(h *identity.AddressHolder) { h.PersonalEmail = "damgasiz@example.org" })
	f.edit("badStamp", func(h *identity.AddressHolder) {
		h.PersonalEmail, h.PersonalEmailVerifiedAt = "bozuk@example.org", "dün"
	})
	f.edit("twinA", func(h *identity.AddressHolder) { h.Email = "ortak@example.test" })
	f.edit("twinB", func(h *identity.AddressHolder) {
		h.PersonalEmail, h.PersonalEmailVerifiedAt = "ORTAK@example.test", "2026-10-01T08:00:00Z"
	})
	f.edit("disabled", func(h *identity.AddressHolder) { h.Enabled = false })
	f.accounts.blocked[f.p["erased"]] = true
	f.edit("service", func(h *identity.AddressHolder) { h.ServiceAccount = true })
	return f
}

func (f *recipientsFixture) now() time.Time {
	f.clockMu.Lock()
	defer f.clockMu.Unlock()
	return f.clock
}

func (f *recipientsFixture) advance(d time.Duration) {
	f.clockMu.Lock()
	defer f.clockMu.Unlock()
	f.clock = f.clock.Add(d)
}

func (f *recipientsFixture) add(name string, change func(*identity.AddressHolder)) uuid.UUID {
	id := uuid.MustParse(fmt.Sprintf("00000000-0000-4000-8000-%012d", len(f.p)+1))
	f.p[name] = id
	h := identity.AddressHolder{ID: id, Enabled: true, Email: name + "@example.test", EmailVerified: true}
	if change != nil {
		change(&h)
	}
	f.book.set(func(b *fakeAddressBook) { b.holders = append(b.holders, h) })
	return id
}

func (f *recipientsFixture) edit(name string, change func(*identity.AddressHolder)) {
	f.book.set(func(b *fakeAddressBook) {
		for i := range b.holders {
			if b.holders[i].ID == f.p[name] {
				change(&b.holders[i])
			}
		}
	})
}

func (f *recipientsFixture) choose(name string, c Category, on bool) {
	f.t.Helper()
	if err := f.store.Apply(context.Background(), f.p[name], []Change{{Channel: ChannelEmail, Category: c, Enabled: &on}}, SourceAccountCenter, f.now()); err != nil {
		f.t.Fatal(err)
	}
}

func (f *recipientsFixture) ask(cat Category, queries ...RecipientQuery) []RecipientDecision {
	f.t.Helper()
	out, err := f.recipients.Resolve(context.Background(), ChannelEmail, cat, queries)
	if err != nil {
		f.t.Fatalf("resolve %v: %v", cat, err)
	}
	if len(out) != len(queries) {
		f.t.Fatalf("%d answers for %d recipients", len(out), len(queries))
	}
	return out
}

func (f *recipientsFixture) one(cat Category, q RecipientQuery) RecipientDecision {
	f.t.Helper()
	return f.ask(cat, q)[0]
}

func email(address string) RecipientQuery { return RecipientQuery{Email: address} }
func sub(id uuid.UUID) RecipientQuery     { return RecipientQuery{Sub: id} }

// waitRead waits for the directory read in flight, if any.
func waitRead(r *Recipients) {
	r.mu.Lock()
	read := r.inflight
	r.mu.Unlock()
	if read != nil {
		<-read.done
	}
}

func (f *recipientsFixture) wantAllowed(d RecipientDecision, name string) {
	f.t.Helper()
	if !d.Matched || d.Sub != f.p[name] || !d.Allowed || d.Reason != "" || d.UnsubscribeURL == "" {
		f.t.Fatalf("%s: %+v", name, d)
	}
}

func (f *recipientsFixture) wantRefused(d RecipientDecision, name string, reason RecipientReason) {
	f.t.Helper()
	if !d.Matched || d.Sub != f.p[name] || d.Allowed || d.Reason != reason || d.UnsubscribeURL != "" {
		f.t.Fatalf("%s: want %s, got %+v", name, reason, d)
	}
}

func wantUnmatched(t *testing.T, what string, d RecipientDecision) {
	t.Helper()
	if d != (RecipientDecision{}) {
		t.Fatalf("%s matched: %+v", what, d)
	}
}

// The login form's identifiers, and only those, are a person's address: a
// verified Primary e-mail, the School e-mail of an account linked to YTÜ
// (from the identity provider read, not core's shadow: ytu never signed in
// to core), a stamped Personal e-mail. Values are compared trimmed and
// lower case on both sides.
func TestRecipientsMatchVerifiedAddresses(t *testing.T) {
	f := newRecipientsFixture(t)
	club := Club(KindAnnouncement)
	for name, address := range map[string]string{
		"member":   "member@example.test",
		"guest":    "  GUEST@Example.Test ",
		"ytu":      "okul.adi@std.yildiz.edu.tr",
		"personal": "Kisisel@Example.ORG",
	} {
		f.wantAllowed(f.one(club, email(address)), name)
	}
	for what, address := range map[string]string{
		"nobody":                   "kimse@example.com",
		"unverified primary":       "unverified@example.test",
		"unlinked school":          "unlinked@std.yildiz.edu.tr",
		"unstamped personal":       "damgasiz@example.org",
		"unreadable stamp":         "bozuk@example.org",
		"service account":          "service@example.test",
		"no account at the domain": "member@example",
	} {
		wantUnmatched(t, what, f.one(club, email(address)))
	}
	f.wantAllowed(f.one(club, sub(f.p["guest"])), "guest")
	wantUnmatched(t, "unknown sub", f.one(club, sub(uuid.New())))
	wantUnmatched(t, "service sub", f.one(club, sub(f.p["service"])))
}

// An address two accounts hold is matched to nobody's preference: no sub,
// never allowed, whatever their choices.
func TestRecipientsAmbiguousAddress(t *testing.T) {
	f := newRecipientsFixture(t)
	d := f.one(Club(KindAnnouncement), email("ortak@example.test"))
	if !d.Matched || d.Sub != uuid.Nil || d.Allowed || d.Reason != RecipientAmbiguous || d.UnsubscribeURL != "" {
		t.Fatalf("%+v", d)
	}
	// Each one's own address and sub still answer for them.
	f.wantAllowed(f.one(Club(KindAnnouncement), sub(f.p["twinB"])), "twinB")
}

// Offset paging answering one account twice does not make its address
// ambiguous: the directory holds a set of subs per address.
func TestRecipientsRepeatedRowIsNoTwin(t *testing.T) {
	f := newRecipientsFixture(t)
	f.book.repeat = true
	for name := range f.p {
		if name == "twinA" || name == "twinB" || name == "service" || name == "unverified" {
			continue
		}
		d := f.one(Club(KindAnnouncement), sub(f.p[name]))
		if d.Reason == RecipientAmbiguous {
			t.Fatalf("%s: %+v", name, d)
		}
	}
	for _, name := range []string{"member", "guest", "ytu", "unlinked", "personal"} {
		if d := f.one(Club(KindAnnouncement), email(name+"@example.test")); d.Reason == RecipientAmbiguous || !d.Matched {
			t.Fatalf("%s: %+v", name, d)
		}
	}
}

func TestRecipientsInactive(t *testing.T) {
	f := newRecipientsFixture(t)
	f.wantRefused(f.one(Club(KindAnnouncement), email("disabled@example.test")), "disabled", RecipientInactive)
	f.wantRefused(f.one(Club(KindAnnouncement), sub(f.p["erased"])), "erased", RecipientInactive)
	// Inactive comes before an opt-out.
	f.choose("disabled", Master(), false)
	f.wantRefused(f.one(Club(KindAnnouncement), email("disabled@example.test")), "disabled", RecipientInactive)
}

// Only the person's own "off" holds a mail back: the master switch, the
// club choice, or the most specific choice of the kind from the team up.
// A default never does (ADR 0072 karar 3).
func TestRecipientsOptOuts(t *testing.T) {
	f := newRecipientsFixture(t)
	duyuru := Team("g-front", KindAnnouncement)

	f.choose("member", Master(), false)
	f.wantRefused(f.one(Club(KindAnnouncement), email("member@example.test")), "member", RecipientOptedOut)
	f.wantRefused(f.one(duyuru, email("member@example.test")), "member", RecipientOptedOut)

	f.choose("guest", Club(KindAnnouncement), false)
	f.wantRefused(f.one(Club(KindAnnouncement), email("guest@example.test")), "guest", RecipientOptedOut)
	f.wantAllowed(f.one(Club(KindNews), email("guest@example.test")), "guest")

	// Off above, nothing on the team: inherited.
	f.choose("personal", Team("g-weblab", KindAnnouncement), false)
	f.wantRefused(f.one(duyuru, sub(f.p["personal"])), "personal", RecipientOptedOut)
	f.wantAllowed(f.one(Team("g-airlab", KindAnnouncement), sub(f.p["personal"])), "personal")
	f.wantAllowed(f.one(Team("g-front", KindNews), sub(f.p["personal"])), "personal")
	// On on the sub-team itself wins over off above.
	f.choose("unlinked", Team("g-weblab", KindAnnouncement), false)
	f.choose("unlinked", duyuru, true)
	f.wantAllowed(f.one(duyuru, sub(f.p["unlinked"])), "unlinked")

	// No choice at all: allowed for a guest (no Member) club-wide, and in
	// a team nobody is in, a hidden one too (O2 is a default).
	for _, cat := range []Category{Club(KindAnnouncement), Club(KindNews), duyuru, Team("g-gizli", KindNews), Team("g-projex", KindAnnouncement)} {
		f.wantAllowed(f.one(cat, sub(f.p["ytu"])), "ytu")
	}
	// An explicit off above a hidden team holds for someone not in it.
	f.choose("ytu", Team("g-weblab", KindAnnouncement), false)
	f.wantRefused(f.one(Team("g-projex", KindAnnouncement), sub(f.p["ytu"])), "ytu", RecipientOptedOut)
}

// The link answered turns exactly the category off: the next question for
// the same person answers opted_out, another category stays allowed.
func TestRecipientsLinkClosesOnlyTheCategory(t *testing.T) {
	f := newRecipientsFixture(t)
	cat := Team("g-weblab", KindNews)
	d := f.one(cat, email("Kisisel@example.org"))
	f.wantAllowed(d, "personal")
	link, err := f.prefs.ReadLink(context.Background(), linkToken(t, d.UnsubscribeURL))
	if err != nil || link.Person != f.p["personal"] || len(link.Categories) != 1 || link.Categories[0].Category != cat || link.Channel != ChannelEmail {
		t.Fatalf("link %+v %v", link, err)
	}
	if _, err := f.prefs.Unsubscribe(context.Background(), linkToken(t, d.UnsubscribeURL), ReachCategories); err != nil {
		t.Fatal(err)
	}
	f.wantRefused(f.one(cat, email("kisisel@example.org")), "personal", RecipientOptedOut)
	f.wantAllowed(f.one(Team("g-weblab", KindAnnouncement), email("kisisel@example.org")), "personal")
	f.wantAllowed(f.one(Club(KindNews), email("kisisel@example.org")), "personal")
}

// The answers come in the order asked, one per recipient, addresses and
// subs mixed; a person asked twice (by address and by sub) is answered
// twice.
func TestRecipientsOrder(t *testing.T) {
	f := newRecipientsFixture(t)
	f.choose("guest", Master(), false)
	out := f.ask(Club(KindAnnouncement),
		email("kimse@example.com"), sub(f.p["member"]), email("guest@example.test"), email("ortak@example.test"),
		sub(f.p["disabled"]), email("member@example.test"))
	wantUnmatched(t, "0", out[0])
	f.wantAllowed(out[1], "member")
	f.wantRefused(out[2], "guest", RecipientOptedOut)
	if out[3].Reason != RecipientAmbiguous {
		t.Fatalf("3: %+v", out[3])
	}
	f.wantRefused(out[4], "disabled", RecipientInactive)
	f.wantAllowed(out[5], "member")
}

// 1000 addresses without an account are answered from the directory: the
// reads are the directory's pages, never one per address, and a second
// question reads nothing.
func TestRecipientsThousandUnknownAddressesReadNoAddress(t *testing.T) {
	f := newRecipientsFixture(t)
	queries := make([]RecipientQuery, MaxRecipients)
	for i := range queries {
		queries[i] = email(fmt.Sprintf("dis%04d@example.com", i))
	}
	started := time.Now()
	out := f.ask(Club(KindAnnouncement), queries...)
	cold := time.Since(started)
	pages, links := f.book.reads()
	// 13 accounts, 3 a page: 5 pages; 1 link, one page.
	if pages != 5 || links != 1 {
		t.Fatalf("reads %d pages, %d link pages", pages, links)
	}
	for i, d := range out {
		wantUnmatched(t, fmt.Sprint(i), d)
	}
	started = time.Now()
	f.ask(Club(KindAnnouncement), queries...)
	warm := time.Since(started)
	if p, l := f.book.reads(); p != pages || l != links {
		t.Fatalf("second question read again: %d %d", p, l)
	}
	t.Logf("1000 addresses without an account: cold %v, warm %v", cold, warm)
}

// No question, no read: there is no timer. The directory is read on the
// first question, not before, and not again until one finds it stale.
func TestRecipientsReadOnlyOnQuestion(t *testing.T) {
	f := newRecipientsFixture(t)
	f.advance(time.Hour)
	if p, l := f.book.reads(); p != 0 || l != 0 {
		t.Fatalf("read without a question: %d %d", p, l)
	}
	f.one(Club(KindAnnouncement), email("member@example.test"))
	pages, links := f.book.reads()
	f.advance(AddressDirectoryFresh - time.Second)
	f.one(Club(KindAnnouncement), email("member@example.test"))
	waitRead(f.recipients)
	if p, l := f.book.reads(); p != pages || l != links {
		t.Fatalf("fresh directory read again: %d %d", p, l)
	}
	f.advance(10 * AddressDirectoryMaxAge)
	if p, l := f.book.reads(); p != pages || l != links {
		t.Fatalf("read without a question: %d %d", p, l)
	}
}

// Concurrent first questions share one read.
func TestRecipientsConcurrentQuestionsShareOneRead(t *testing.T) {
	f := newRecipientsFixture(t)
	gate, started := make(chan struct{}), make(chan struct{}, 16)
	f.book.set(func(b *fakeAddressBook) { b.gate, b.started = gate, started })
	const callers = 8
	var wg sync.WaitGroup
	errs := make(chan error, callers)
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := f.recipients.Resolve(context.Background(), ChannelEmail, Club(KindAnnouncement), []RecipientQuery{email("member@example.test")})
			if err == nil && !out[0].Allowed {
				err = fmt.Errorf("answer %+v", out[0])
			}
			errs <- err
		}()
	}
	<-started
	// Every caller is waiting on the read (or has not reached it): let it
	// end once they have had the time to join.
	time.Sleep(50 * time.Millisecond)
	f.book.set(func(b *fakeAddressBook) { b.started = nil })
	close(gate)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, links := f.book.reads(); links != 1 {
		t.Fatalf("%d reads", links)
	}
}

// A stale directory (past 5 minutes) answers at once and starts one read;
// the next question sees what it read.
func TestRecipientsStaleDirectoryAnswersAndRefreshes(t *testing.T) {
	f := newRecipientsFixture(t)
	f.wantAllowed(f.one(Club(KindAnnouncement), email("member@example.test")), "member")
	newcomer := f.add("newcomer", nil)
	f.advance(AddressDirectoryFresh)
	// Answered from the old directory: the newcomer is not in it yet.
	wantUnmatched(t, "newcomer before the read", f.one(Club(KindAnnouncement), email("newcomer@example.test")))
	waitRead(f.recipients)
	d := f.one(Club(KindAnnouncement), email("newcomer@example.test"))
	if !d.Allowed || d.Sub != newcomer {
		t.Fatalf("after the read: %+v", d)
	}
}

// A read that fails halfway leaves the directory as it was: nothing of
// the half read is used. Past 15 minutes without a read, the question is
// refused; a directory never read is refused too.
func TestRecipientsFailedReadKeepsTheOldDirectory(t *testing.T) {
	f := newRecipientsFixture(t)
	f.wantAllowed(f.one(Club(KindAnnouncement), email("member@example.test")), "member")
	pagesBefore, _ := f.book.reads()

	// member leaves (first page), a newcomer arrives (last page), and the
	// second page fails.
	f.edit("member", func(h *identity.AddressHolder) { h.Email = "gitti@example.test" })
	f.book.set(func(b *fakeAddressBook) { b.failAt = 3 })
	f.add("late", nil)
	f.advance(AddressDirectoryFresh)
	f.wantAllowed(f.one(Club(KindAnnouncement), email("member@example.test")), "member")
	waitRead(f.recipients)
	if pages, _ := f.book.reads(); pages != pagesBefore+2 {
		t.Fatalf("pages %d → %d", pagesBefore, pages)
	}
	f.wantAllowed(f.one(Club(KindAnnouncement), email("member@example.test")), "member")
	wantUnmatched(t, "half read", f.one(Club(KindAnnouncement), email("gitti@example.test")))

	// A failed read is not tried again at once.
	f.advance(addressDirectoryRetry / 2)
	f.one(Club(KindAnnouncement), email("member@example.test"))
	waitRead(f.recipients)
	if pages, _ := f.book.reads(); pages != pagesBefore+2 {
		t.Fatalf("tried again at once: %d", pages)
	}

	f.advance(AddressDirectoryMaxAge)
	_, err := f.recipients.Resolve(context.Background(), ChannelEmail, Club(KindAnnouncement), []RecipientQuery{email("member@example.test")})
	if !errors.Is(err, ErrAddressDirectoryUnavailable) {
		t.Fatalf("15 minutes old: %v", err)
	}
	// Keycloak answers again: the next question reads and answers.
	f.book.set(func(b *fakeAddressBook) { b.failAt = -1 })
	f.advance(addressDirectoryRetry)
	wantUnmatched(t, "member gone", f.one(Club(KindAnnouncement), email("member@example.test")))
	f.wantAllowed(f.one(Club(KindAnnouncement), email("gitti@example.test")), "member")

	never := newRecipientsFixture(t)
	never.book.failLinks = true
	if _, err := never.recipients.Resolve(context.Background(), ChannelEmail, Club(KindAnnouncement), []RecipientQuery{email("member@example.test")}); !errors.Is(err, ErrAddressDirectoryUnavailable) {
		t.Fatalf("never read: %v", err)
	}
}

func TestRecipientsRefusals(t *testing.T) {
	f := newRecipientsFixture(t)
	ctx := context.Background()
	one := []RecipientQuery{email("member@example.test")}
	many := make([]RecipientQuery, MaxRecipients+1)
	for i := range many {
		many[i] = email(fmt.Sprintf("a%d@example.com", i))
	}
	for name, c := range map[string]struct {
		ch  Channel
		cat Category
		q   []RecipientQuery
	}{
		"master switch": {ChannelEmail, Master(), one},
		"no category":   {ChannelEmail, Category{}, one},
		"other channel": {Channel("sms"), Club(KindAnnouncement), one},
		"none":          {ChannelEmail, Club(KindAnnouncement), nil},
		"too many":      {ChannelEmail, Club(KindAnnouncement), many},
		"both fields":   {ChannelEmail, Club(KindAnnouncement), []RecipientQuery{{Email: "a@example.com", Sub: uuid.New()}}},
		"neither":       {ChannelEmail, Club(KindAnnouncement), []RecipientQuery{{}}},
		"blank address": {ChannelEmail, Club(KindAnnouncement), []RecipientQuery{email("   ")}},
		"no at":         {ChannelEmail, Club(KindAnnouncement), []RecipientQuery{email("member")}},
		"same address":  {ChannelEmail, Club(KindAnnouncement), []RecipientQuery{email("a@example.com"), email(" A@Example.com")}},
		"same sub":      {ChannelEmail, Club(KindAnnouncement), []RecipientQuery{sub(f.p["member"]), sub(f.p["member"])}},
		"long address":  {ChannelEmail, Club(KindAnnouncement), []RecipientQuery{email(string(make([]byte, 320)) + "@x")}},
	} {
		if _, err := f.recipients.Resolve(ctx, c.ch, c.cat, c.q); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// Not a team's own Group: an area, a leader subgroup, a Privileged
	// root, an unknown id. A hidden team is a team.
	for _, id := range []string{"g-arge", "g-weblab-l", "g-yk", "g-yk-alt", "missing"} {
		if _, err := f.recipients.Resolve(ctx, ChannelEmail, Team(id, KindAnnouncement), one); !errors.Is(err, ErrUnknownTeam) {
			t.Fatalf("%s: %v", id, err)
		}
	}
	if p, l := f.book.reads(); p != 0 || l != 0 {
		t.Fatalf("a refused question read the directory: %d %d", p, l)
	}
	if _, err := f.recipients.Resolve(ctx, ChannelEmail, Team("g-projex", KindAnnouncement), one); err != nil {
		t.Fatal(err)
	}
	// Keycloak's Group list not answering is the directory's error.
	f.kc.mu.Lock()
	f.kc.fail = errKeycloakDown
	f.kc.mu.Unlock()
	f.advance(TreeTTL)
	if _, err := f.recipients.Resolve(ctx, ChannelEmail, Team("g-front", KindAnnouncement), one); !errors.Is(err, ErrTeamsUnavailable) {
		t.Fatalf("tree: %v", err)
	}
	// Without the link key: off.
	off := NewRecipients(NewService(Config{}, f.store, f.kc, nil), f.book, f.accounts, testYTUAlias)
	if _, err := off.Resolve(ctx, ChannelEmail, Club(KindAnnouncement), one); !errors.Is(err, ErrDisabled) || off.Enabled() {
		t.Fatalf("off: %v", err)
	}
	var none *Recipients
	if none.Enabled() {
		t.Fatal("nil enabled")
	}
}

func TestYTUIdentityProviderFromEnv(t *testing.T) {
	for raw, want := range map[string]string{"": "OBS", "  ": "OBS", "OBS": "OBS", " ytu-microsoft ": "ytu-microsoft"} {
		got, err := YTUIdentityProviderFromEnv(func(string) string { return raw })
		if err != nil || got != want {
			t.Fatalf("%q: %q %v", raw, got, err)
		}
	}
	for _, raw := range []string{"a b", "-x", "o/b", "ö"} {
		if _, err := YTUIdentityProviderFromEnv(func(string) string { return raw }); err == nil {
			t.Fatalf("%q accepted", raw)
		}
	}
}

// logLines collects what the directory logs.
type logLines struct {
	mu    sync.Mutex
	lines []string
}

func (l *logLines) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logLines) take() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := l.lines
	l.lines = nil
	return out
}

// Every directory read logs one line of counts, never an address or a
// subject; a failed read in the background logs its error.
func TestRecipientsDirectoryReadLogsCounts(t *testing.T) {
	f := newRecipientsFixture(t)
	var logs logLines
	f.recipients.SetLogf(logs.logf)
	f.one(Club(KindAnnouncement), email("member@example.test"))
	lines := logs.take()
	if len(lines) != 1 {
		t.Fatalf("lines: %q", lines)
	}
	want := `{"event":"recipients_directory_read","ok":true,"ms":`
	if !strings.HasPrefix(lines[0], want) ||
		!strings.HasSuffix(lines[0], `,"accounts":12,"linked":1,"school_email":2,"primary_unverified":1}`) {
		t.Fatalf("line: %s", lines[0])
	}
	for _, name := range []string{"member", "ytu", "unlinked"} {
		if strings.Contains(lines[0], name) || strings.Contains(lines[0], f.p[name].String()) {
			t.Fatalf("names a person: %s", lines[0])
		}
	}

	// The refresh in the background fails: logged, the old directory
	// answers.
	f.book.set(func(b *fakeAddressBook) { b.failAt = 3 })
	f.advance(AddressDirectoryFresh)
	f.wantAllowed(f.one(Club(KindAnnouncement), email("member@example.test")), "member")
	waitRead(f.recipients)
	lines = logs.take()
	if len(lines) != 1 || !strings.HasPrefix(lines[0], `{"event":"recipients_directory_read","level":"warn","ok":false,"ms":`) ||
		!strings.HasSuffix(lines[0], `,"error":"keycloak: status 502"}`) {
		t.Fatalf("failed read: %q", lines)
	}
}

// No account linked to the identity provider while accounts hold a
// School e-mail: the alias is probably wrong, and it is said.
func TestRecipientsDirectoryWarnsOfAnUnlinkedAlias(t *testing.T) {
	f := newRecipientsFixture(t)
	f.recipients = NewRecipients(f.prefs, f.book, f.accounts, "OBS-typo")
	f.recipients.SetClock(f.now)
	f.recipients.SetPageSize(3)
	var logs logLines
	f.recipients.SetLogf(logs.logf)
	wantUnmatched(t, "school e-mail without link", f.one(Club(KindAnnouncement), email("okul.adi@std.yildiz.edu.tr")))
	lines := logs.take()
	if len(lines) != 2 || !strings.Contains(lines[0], `"linked":0,"school_email":2`) ||
		lines[1] != `{"event":"recipients_directory_alias_suspect","level":"warn","alias":"OBS-typo","linked":0,"school_email":2,"hint":"no account is linked to the identity provider but accounts hold a School e-mail: KEYCLOAK_YTU_IDP_ALIAS may be wrong"}` {
		t.Fatalf("lines: %q", lines)
	}

	// Nobody holds a School e-mail: no warning.
	none := newRecipientsFixture(t)
	none.book.linked = nil
	for _, name := range []string{"ytu", "unlinked"} {
		none.edit(name, func(h *identity.AddressHolder) { h.SchoolEmail = "" })
	}
	none.recipients.SetLogf(logs.logf)
	none.one(Club(KindAnnouncement), email("member@example.test"))
	if lines := logs.take(); len(lines) != 1 {
		t.Fatalf("lines: %q", lines)
	}
}
