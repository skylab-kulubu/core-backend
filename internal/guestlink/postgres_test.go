package guestlink_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/commpref"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/guestlink"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// world is two Events and these people:
//
//	ada: active; Primary ada@example.test, School ada@std.yildiz.edu.tr
//	bob: active, holds a member Ticket on e1; Personal bob@kisisel.org
//	cem: active; Primary cem@example.test, Personal cem@kisisel.org
//	dan: an erasure requested; dan@example.test
//	ortak@example.test: verified by ada and bob (ambiguous)
//	unverified@example.test: nobody's verified address (the directory
//	  leaves an unverified address out)
type world struct {
	pool                     *pgxpool.Pool
	store                    *guestlink.Postgres
	tickets                  *ticket.PostgresStore
	ada, bob, cem, dan       uuid.UUID
	e1, e2                   uuid.UUID
	addrs                    map[string]commpref.VerifiedAddress
	adaE1, adaE2, bobE1      uuid.UUID
	cemE1a, cemE1b           uuid.UUID
	twinE1, unverE1, danE1   uuid.UUID
	bobMember                uuid.UUID
	certAda, certBob         uuid.UUID
	certTwin, certUnverified uuid.UUID
}

func exec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", strings.Fields(sql)[0], err)
	}
}

func newWorld(t *testing.T) *world {
	t.Helper()
	ctx := context.Background()
	pool := testpostgres.Start(t)
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	w := &world{pool: pool, store: guestlink.NewPostgres(pool), tickets: ticket.NewPostgresStore(pool),
		ada: uuid.New(), bob: uuid.New(), cem: uuid.New(), dan: uuid.New(), e1: uuid.New(), e2: uuid.New()}
	users := user.NewPostgresStore(pool)
	for name, id := range map[string]uuid.UUID{"ada": w.ada, "bob": w.bob, "cem": w.cem, "dan": w.dan} {
		if _, _, err := user.NewService(users).Ensure(ctx, id, user.Profile{Email: name + "@example.test", FirstName: name, LastName: "X"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := users.RequestDeletion(ctx, w.dan, nil); err != nil {
		t.Fatal(err)
	}
	for _, e := range []uuid.UUID{w.e1, w.e2} {
		exec(t, pool, `INSERT INTO events (id, name, location, owner_team) VALUES ($1, 'Hack', 'YTÜ', 'WEBLAB')`, e)
	}
	member, err := w.tickets.Create(ctx, ticket.Ticket{EventID: w.e1, TicketType: ticket.Registered, OwnerID: &w.bob})
	if err != nil {
		t.Fatal(err)
	}
	w.bobMember = member.ID
	guest := func(event uuid.UUID, email, first, last string) uuid.UUID {
		t.Helper()
		got, err := w.tickets.Create(ctx, ticket.Ticket{EventID: event, TicketType: ticket.Guest, GuestFirstName: first, GuestLastName: last, GuestEmail: email, GuestPhoneNumber: "+905550001122"})
		if err != nil {
			t.Fatal(err)
		}
		return got.ID
	}
	w.adaE1 = guest(w.e1, "Ada@Example.test", "Ada", "Konuk")
	w.adaE2 = guest(w.e2, "ada@std.yildiz.edu.tr", "Ada", "Okul")
	w.bobE1 = guest(w.e1, "bob@kisisel.org", "Bob", "Konuk")
	w.cemE1a = guest(w.e1, "cem@example.test", "Cem", "Bir")
	w.cemE1b = guest(w.e1, "cem@kisisel.org", "Cem", "İki")
	w.twinE1 = guest(w.e1, "ortak@example.test", "Ortak", "Adres")
	w.unverE1 = guest(w.e1, "unverified@example.test", "Doğrulanmamış", "Adres")
	w.danE1 = guest(w.e1, "dan@example.test", "Dan", "Silinen")
	cert := func(ticketID uuid.UUID, email string) uuid.UUID {
		t.Helper()
		id := uuid.New()
		exec(t, pool, `
			INSERT INTO certificates (id, event_id, ticket_id, serial, recipient_name, recipient_email, event_name, owner_team, verify_url)
			SELECT $1, event_id, id, $2, 'Ad Soyad', $3, 'Hack', 'WEBLAB', 'https://example.test/c/' || $2 FROM tickets WHERE id = $4`,
			id, strings.ToUpper(strings.ReplaceAll(id.String(), "-", "")), email, ticketID)
		return id
	}
	w.certAda = cert(w.adaE1, "ada@example.test")
	w.certBob = cert(w.bobE1, "bob@kisisel.org")
	w.certTwin = cert(w.twinE1, "ortak@example.test")
	w.certUnverified = cert(w.unverE1, "unverified@example.test")
	one := func(id uuid.UUID, kind commpref.AddressKind) commpref.VerifiedAddress {
		return commpref.VerifiedAddress{Holders: []uuid.UUID{id}, Kind: kind, Enabled: true}
	}
	w.addrs = map[string]commpref.VerifiedAddress{
		"ada@example.test":      one(w.ada, commpref.AddressPrimary),
		"ada@std.yildiz.edu.tr": one(w.ada, commpref.AddressSchool),
		"bob@example.test":      one(w.bob, commpref.AddressPrimary),
		"bob@kisisel.org":       one(w.bob, commpref.AddressPersonal),
		"cem@example.test":      one(w.cem, commpref.AddressPrimary),
		"cem@kisisel.org":       one(w.cem, commpref.AddressPersonal),
		"dan@example.test":      one(w.dan, commpref.AddressPrimary),
		"ortak@example.test":    {Holders: []uuid.UUID{w.ada, w.bob}, Kind: commpref.AddressPrimary, Enabled: true},
	}
	return w
}

func (w *world) owner(t *testing.T, table string, id uuid.UUID) (*uuid.UUID, *string) {
	t.Helper()
	var owner *uuid.UUID
	var source *string
	if err := w.pool.QueryRow(context.Background(), `SELECT owner_id, owner_link_source FROM `+table+` WHERE id = $1`, id).Scan(&owner, &source); err != nil {
		t.Fatal(err)
	}
	return owner, source
}

func (w *world) wantOwner(t *testing.T, table, what string, id uuid.UUID, want *uuid.UUID, source string) {
	t.Helper()
	owner, got := w.owner(t, table, id)
	switch {
	case want == nil && owner != nil:
		t.Fatalf("%s: owned by %s", what, owner)
	case want != nil && (owner == nil || *owner != *want):
		t.Fatalf("%s: owner %v, want %s", what, owner, want)
	case source == "" && got != nil, source != "" && (got == nil || *got != source):
		t.Fatalf("%s: source %v, want %q", what, got, source)
	}
}

// A dry run counts and writes nothing; the run links each row once, by the
// rule; a second run links nothing more; the door still finds every linked
// Ticket by the guest's e-mail, name and code; the person's own list holds
// them; taking it back restores every owner.
func TestRunLinksByTheRuleOnceAndTakesBack(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)

	dry, err := guestlink.Run(ctx, w.store, w.addrs, ticket.LinkBulk, false)
	if err != nil {
		t.Fatal(err)
	}
	tc := dry.Tickets
	if tc.Linked[guestlink.KindPrimary] != 2 || tc.Linked[guestlink.KindSchool] != 1 || tc.Linked[guestlink.KindPersonal] != 0 ||
		tc.Ambiguous != 1 || tc.Duplicate != 2 || tc.Inactive != 1 || tc.Unmatched != 1 {
		t.Fatalf("dry run tickets %+v", tc)
	}
	cc := dry.Certificates
	if cc.Linked[guestlink.KindTicket] != 1 || cc.Linked[guestlink.KindPersonal] != 1 || cc.Ambiguous != 1 || cc.Unmatched != 1 {
		t.Fatalf("dry run certificates %+v", cc)
	}
	for _, id := range []uuid.UUID{w.adaE1, w.adaE2, w.cemE1a, w.cemE1b} {
		w.wantOwner(t, "tickets", "dry run", id, nil, "")
	}
	w.wantOwner(t, "certificates", "dry run", w.certAda, nil, "")

	applied, err := guestlink.Run(ctx, w.store, w.addrs, ticket.LinkBulk, true)
	if err != nil {
		t.Fatal(err)
	}
	if applied.Tickets.Total() != 3 || applied.Certificates.Total() != 2 || applied.Tickets.Changed != 0 {
		t.Fatalf("applied %+v", applied)
	}
	w.wantOwner(t, "tickets", "ada primary", w.adaE1, &w.ada, "bulk")
	w.wantOwner(t, "tickets", "ada school", w.adaE2, &w.ada, "bulk")
	w.wantOwner(t, "tickets", "cem's older one", w.cemE1a, &w.cem, "bulk")
	w.wantOwner(t, "tickets", "cem's second address, same Event", w.cemE1b, nil, "")
	w.wantOwner(t, "tickets", "bob holds a member Ticket", w.bobE1, nil, "")
	w.wantOwner(t, "tickets", "bob's member Ticket", w.bobMember, &w.bob, "")
	w.wantOwner(t, "tickets", "ambiguous", w.twinE1, nil, "")
	w.wantOwner(t, "tickets", "unverified", w.unverE1, nil, "")
	w.wantOwner(t, "tickets", "erasure requested", w.danE1, nil, "")
	w.wantOwner(t, "certificates", "by its Ticket", w.certAda, &w.ada, "bulk")
	w.wantOwner(t, "certificates", "duplicate Ticket's, by its address", w.certBob, &w.bob, "bulk")
	w.wantOwner(t, "certificates", "ambiguous", w.certTwin, nil, "")
	w.wantOwner(t, "certificates", "unverified", w.certUnverified, nil, "")

	// The linked Ticket is the guest's still.
	got, err := w.tickets.Get(ctx, w.adaE1)
	if err != nil || got.TicketType != ticket.Guest || got.GuestEmail != "Ada@Example.test" || got.GuestFirstName != "Ada" || got.GuestPhoneNumber == "" {
		t.Fatalf("linked Ticket %+v %v", got, err)
	}
	for _, q := range []string{"ada@example.test", "Ada Konuk"} {
		found, err := w.tickets.SearchDoorTickets(ctx, w.e1, q, true, 2)
		if err != nil || len(found) != 1 || found[0].Ticket.ID != w.adaE1 || found[0].Email != "Ada@Example.test" || found[0].Name != "Ada Konuk" {
			t.Fatalf("door search %q: %+v %v", q, found, err)
		}
	}
	if found, err := w.tickets.SearchDoorTickets(ctx, w.e1, "konuk", false, 20); err != nil || len(found) != 2 {
		t.Fatalf("door partial search: %+v %v", found, err)
	}
	if row, err := w.tickets.DoorTicketByCode(ctx, w.e1, strings.ReplaceAll(got.Code, "-", "")); err != nil || row.Ticket.ID != w.adaE1 {
		t.Fatalf("door code: %+v %v", row, err)
	}
	if found, err := w.tickets.DoorTicketsByOwners(ctx, w.e1, []uuid.UUID{w.ada}); err != nil || len(found) != 1 || found[0].Ticket.ID != w.adaE1 || found[0].Name != "Ada Konuk" {
		t.Fatalf("door by person: %+v %v", found, err)
	}
	if byEmail, err := w.tickets.GetByGuestEvent(ctx, "ada@example.test", w.e1); err != nil || byEmail.ID != w.adaE1 {
		t.Fatalf("guest lookup: %+v %v", byEmail, err)
	}
	if listed, err := w.tickets.ListByGuestEmail(ctx, "ada@example.test"); err != nil || len(listed) != 1 {
		t.Fatalf("self check-in lookup: %+v %v", listed, err)
	}
	mine, err := w.tickets.ListByOwner(ctx, w.ada)
	if err != nil || len(mine) != 2 {
		t.Fatalf("Biletlerim: %d %v", len(mine), err)
	}

	again, err := guestlink.Run(ctx, w.store, w.addrs, ticket.LinkSync, true)
	if err != nil {
		t.Fatal(err)
	}
	if again.Tickets.Total() != 0 || again.Certificates.Total() != 0 {
		t.Fatalf("a second run linked again: %+v", again)
	}
	w.wantOwner(t, "tickets", "kept its first source", w.adaE1, &w.ada, "bulk")

	// A certificate issued from a linked Ticket after the link takes its
	// owner, as every certificate does; taking back takes it back too.
	issued := uuid.New()
	exec(t, w.pool, `
		INSERT INTO certificates (id, event_id, ticket_id, owner_id, serial, recipient_name, recipient_email, event_name, owner_team, verify_url, revoked_at)
		VALUES ($1, $2, $3, $4, 'ISSUEDAFTER', 'Ada Okul', 'ada@std.yildiz.edu.tr', 'Hack', 'WEBLAB', 'https://example.test/c/x', NULL)`,
		issued, w.e2, w.adaE2, w.ada)
	undone, err := w.store.Unlink(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if undone.Tickets != 3 || undone.Certificates != 2 || undone.IssuedFromLinked != 1 {
		t.Fatalf("undone %+v", undone)
	}
	for _, id := range []uuid.UUID{w.adaE1, w.adaE2, w.cemE1a} {
		w.wantOwner(t, "tickets", "after undo", id, nil, "")
	}
	w.wantOwner(t, "tickets", "a member's own Ticket stays", w.bobMember, &w.bob, "")
	for _, id := range []uuid.UUID{w.certAda, w.certBob, issued} {
		w.wantOwner(t, "certificates", "after undo", id, nil, "")
	}
	if on, err := w.store.Enabled(ctx); err != nil || on {
		t.Fatalf("switch after undo: %t %v", on, err)
	}
}

// fakeDirectory answers from a fixed map.
type fakeDirectory struct {
	addrs map[string]commpref.VerifiedAddress
	fail  bool
}

func (d fakeDirectory) VerifiedAddressOf(_ context.Context, address string) (commpref.VerifiedAddress, bool, error) {
	if d.fail {
		return commpref.VerifiedAddress{}, false, commpref.ErrAddressDirectoryUnavailable
	}
	v, ok := d.addrs[address]
	return v, ok, nil
}

func (d fakeDirectory) FreshVerifiedAddresses(context.Context) (map[string]commpref.VerifiedAddress, error) {
	if d.fail {
		return nil, commpref.ErrAddressDirectoryUnavailable
	}
	return d.addrs, nil
}

// The write's lookup links nothing while the switch is off, and once on
// names the one active account without a Ticket on the Event; a directory
// that does not answer names nobody.
func TestResolverFollowsTheSwitchAndTheRule(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	r := guestlink.NewResolver(w.store, fakeDirectory{addrs: w.addrs}, nil)
	if _, ok := r.GuestOwner(ctx, w.e2, "cem@kisisel.org"); ok {
		t.Fatal("linked while the switch is off")
	}
	if err := w.store.SetEnabled(ctx, true); err != nil {
		t.Fatal(err)
	}
	r = guestlink.NewResolver(w.store, fakeDirectory{addrs: w.addrs}, nil)
	if id, ok := r.GuestOwner(ctx, w.e2, " CEM@kisisel.org "); !ok || id != w.cem {
		t.Fatalf("cem on e2: %s %t", id, ok)
	}
	for what, c := range map[string]struct {
		event   uuid.UUID
		address string
	}{
		"holds a Ticket on the Event": {w.e1, "bob@kisisel.org"},
		"ambiguous":                   {w.e2, "ortak@example.test"},
		"erasure requested":           {w.e2, "dan@example.test"},
		"nobody's":                    {w.e2, "unverified@example.test"},
	} {
		if id, ok := r.GuestOwner(ctx, c.event, c.address); ok {
			t.Fatalf("%s: %s", what, id)
		}
	}
	down := guestlink.NewResolver(w.store, fakeDirectory{fail: true}, nil)
	if _, ok := down.GuestOwner(ctx, w.e2, "cem@kisisel.org"); ok {
		t.Fatal("linked without a directory")
	}
	// A guest apply writes the Ticket linked, with the mark.
	svc := ticket.NewService(w.tickets, event.NewPostgresStore(w.pool), authz.NewAuthorizer(authz.DefaultPolicy()), ticket.GuestOwners(r))
	if _, err := svc.ApplyGuest(ctx, authz.Principal{}, w.e2, ticket.GuestInfo{FirstName: "Cem", LastName: "İki", Email: "Cem@Kisisel.org"}); err != nil {
		t.Fatal(err)
	}
	written, err := w.tickets.GetByGuestEvent(ctx, "cem@kisisel.org", w.e2)
	if err != nil {
		t.Fatal(err)
	}
	w.wantOwner(t, "tickets", "written linked", written.ID, &w.cem, "write")
	// bob holds a Ticket on e1: his guest Ticket there is written unlinked.
	if _, err := svc.ApplyGuest(ctx, authz.Principal{}, w.e1, ticket.GuestInfo{FirstName: "Bob", LastName: "B", Email: "bob@example.test"}); err != nil {
		t.Fatal(err)
	}
	unlinked, err := w.tickets.GetByGuestEvent(ctx, "bob@example.test", w.e1)
	if err != nil {
		t.Fatal(err)
	}
	w.wantOwner(t, "tickets", "written unlinked", unlinked.ID, nil, "")
}

// The user sync's step: nothing while off; on, a fresh read links, and a
// directory that fails links nothing and fails nothing.
func TestLinkerFollowsTheSwitch(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	var lines []string
	logf := func(format string, args ...any) { lines = append(lines, format) }
	guestlink.NewLinker(w.store, fakeDirectory{addrs: w.addrs}, logf).LinkGuests(ctx)
	w.wantOwner(t, "tickets", "switch off", w.adaE1, nil, "")
	if err := w.store.SetEnabled(ctx, true); err != nil {
		t.Fatal(err)
	}
	guestlink.NewLinker(w.store, fakeDirectory{fail: true}, logf).LinkGuests(ctx)
	w.wantOwner(t, "tickets", "directory down", w.adaE1, nil, "")
	guestlink.NewLinker(w.store, fakeDirectory{addrs: w.addrs}, logf).LinkGuests(ctx)
	w.wantOwner(t, "tickets", "switch on", w.adaE1, &w.ada, "sync")
	w.wantOwner(t, "certificates", "switch on", w.certAda, &w.ada, "sync")
	if len(lines) != 2 {
		t.Fatalf("log lines %q", lines)
	}
}
