// Package guestlink links guest Tickets and ownerless certificates to the
// account their address is (docs/guest-account-links.md): the one active
// account that holds the address verified (ADR-0044's addresses, read from
// the verified-address directory). The Ticket stays GUEST with its name and
// e-mail; only its owner is written, with a mark that says core linked it.
// Nobody's account is made: an address without one is left as it is.
//
// Three ways link: a new guest Ticket is written linked (Resolver, the
// ticket service's GuestOwners), the user sync's pass links what the
// accounts' addresses now name (Linker), and the one-time command links
// what was there before (Run). One switch, off until the command's -apply,
// turns all three on.
package guestlink

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/commpref"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

// Kind is why a Ticket or certificate goes to an account: the kind of the
// verified address (commpref.AddressKind), or the owner of a certificate's
// Ticket.
type Kind string

const (
	KindPrimary  Kind = Kind(commpref.AddressPrimary)
	KindSchool   Kind = Kind(commpref.AddressSchool)
	KindPersonal Kind = Kind(commpref.AddressPersonal)
	// KindTicket: a certificate whose Ticket has an owner.
	KindTicket Kind = "ticket"
)

// Counts are one kind of row's outcomes. They name nobody.
type Counts struct {
	// Linked are the rows linked (or, in a dry run, to be linked), by why.
	Linked map[Kind]int
	// Ambiguous: two or more accounts hold the address verified.
	Ambiguous int
	// Disabled: the one account is disabled in Keycloak.
	Disabled int
	// Inactive: the account has no active core row (none yet, being erased
	// or erased).
	Inactive int
	// Duplicate: the account already holds a Ticket on the Event (Tickets
	// only); the guest Ticket stays as it is.
	Duplicate int
	// Unmatched: no account holds the address verified.
	Unmatched int
	// Changed: the row changed between the read and the write (another
	// pass, the person, an operator): left for the next pass.
	Changed int
}

// Total is every row linked.
func (c Counts) Total() int {
	n := 0
	for _, v := range c.Linked {
		n += v
	}
	return n
}

func newCounts() Counts { return Counts{Linked: map[Kind]int{}} }

// Report is one run's counts.
type Report struct {
	Tickets      Counts
	Certificates Counts
}

func (r Report) changed() bool { return r.Tickets.Total()+r.Certificates.Total() > 0 }

// line is the report as one JSON log line, counts only.
func (r Report) line(event string, source ticket.LinkSource) string {
	return fmt.Sprintf(`{"event":%q,"source":%q,"tickets_linked":%d,"tickets_primary":%d,"tickets_school":%d,"tickets_personal":%d,"tickets_ambiguous":%d,"tickets_disabled":%d,"tickets_inactive":%d,"tickets_duplicate":%d,"tickets_changed":%d,"certificates_linked":%d,"certificates_ambiguous":%d,"certificates_changed":%d}`,
		event, source, r.Tickets.Total(), r.Tickets.Linked[KindPrimary], r.Tickets.Linked[KindSchool], r.Tickets.Linked[KindPersonal],
		r.Tickets.Ambiguous, r.Tickets.Disabled, r.Tickets.Inactive, r.Tickets.Duplicate, r.Tickets.Changed,
		r.Certificates.Total(), r.Certificates.Ambiguous, r.Certificates.Changed)
}

// GuestTicket is an unlinked guest Ticket with an address.
type GuestTicket struct {
	ID, EventID uuid.UUID
	// Address is the guest e-mail, trimmed and lower case.
	Address   string
	Status    string
	CreatedAt time.Time
}

// OwnerlessCertificate is a certificate without an owner that still has
// its recipient's address.
type OwnerlessCertificate struct {
	ID, TicketID uuid.UUID
	Address      string
	// TicketOwner is its Ticket's owner, nil for none.
	TicketOwner *uuid.UUID
}

// Accounts is what the plan needs of the accounts the addresses name.
type Accounts struct {
	// Active: an active core row and no erasure request (the database's
	// active_account_reference rule).
	Active map[uuid.UUID]bool
	// Holds: the account holds a Ticket on the Event.
	Holds map[[2]uuid.UUID]bool
}

// Link is one row to link.
type Link struct {
	ID, Owner uuid.UUID
	// Address is the Ticket's address as read: the write checks it is
	// still the Ticket's.
	Address string
	Kind    Kind
}

// statusRank orders a person's guest Tickets on one Event: the one linked
// is the active one, else the waitlisted, else the cancelled; the oldest
// among equals.
func statusRank(s string) int {
	switch s {
	case string(ticket.StatusActive), "":
		return 0
	case string(ticket.StatusWaitlisted):
		return 1
	}
	return 2
}

// owner classifies address: the owner and why, or the count it falls in.
func owner(addrs map[string]commpref.VerifiedAddress, accounts Accounts, address string, c *Counts) (uuid.UUID, Kind, bool) {
	v, ok := addrs[address]
	switch {
	case !ok || len(v.Holders) == 0:
		c.Unmatched++
		return uuid.Nil, "", false
	case len(v.Holders) > 1:
		c.Ambiguous++
		return uuid.Nil, "", false
	case !v.Enabled:
		c.Disabled++
		return uuid.Nil, "", false
	case !accounts.Active[v.Holders[0]]:
		c.Inactive++
		return uuid.Nil, "", false
	}
	return v.Holders[0], Kind(v.Kind), true
}

// PlanTickets says which of rows to link and to whom: an address one
// enabled, active account holds verified, and an Event the account holds
// no Ticket on. Of a person's guest Tickets on one Event (two of their
// addresses) one is linked (statusRank), the others are duplicates.
func PlanTickets(rows []GuestTicket, addrs map[string]commpref.VerifiedAddress, accounts Accounts) ([]Link, Counts) {
	counts := newCounts()
	rows = slices.Clone(rows)
	slices.SortStableFunc(rows, func(a, b GuestTicket) int {
		if c := statusRank(a.Status) - statusRank(b.Status); c != 0 {
			return c
		}
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.ID.String(), b.ID.String())
	})
	holds := make(map[[2]uuid.UUID]bool, len(accounts.Holds))
	for k, v := range accounts.Holds {
		holds[k] = v
	}
	var links []Link
	for _, row := range rows {
		id, kind, ok := owner(addrs, accounts, row.Address, &counts)
		if !ok {
			continue
		}
		key := [2]uuid.UUID{id, row.EventID}
		if holds[key] {
			counts.Duplicate++
			continue
		}
		holds[key] = true
		counts.Linked[kind]++
		links = append(links, Link{ID: row.ID, Owner: id, Address: row.Address, Kind: kind})
	}
	return links, counts
}

// PlanCertificates says which ownerless certificates to link: to their
// Ticket's owner (one linked in this run included, ticketLinks), else to
// the account their address is.
func PlanCertificates(rows []OwnerlessCertificate, addrs map[string]commpref.VerifiedAddress, accounts Accounts, ticketLinks map[uuid.UUID]uuid.UUID) ([]Link, Counts) {
	counts := newCounts()
	var links []Link
	for _, row := range rows {
		ticketOwner := row.TicketOwner
		if ticketOwner == nil {
			if o, ok := ticketLinks[row.TicketID]; ok {
				ticketOwner = &o
			}
		}
		if ticketOwner != nil {
			if !accounts.Active[*ticketOwner] {
				counts.Inactive++
				continue
			}
			counts.Linked[KindTicket]++
			links = append(links, Link{ID: row.ID, Owner: *ticketOwner, Address: row.Address, Kind: KindTicket})
			continue
		}
		id, kind, ok := owner(addrs, accounts, row.Address, &counts)
		if !ok {
			continue
		}
		counts.Linked[kind]++
		links = append(links, Link{ID: row.ID, Owner: id, Address: row.Address, Kind: kind})
	}
	return links, counts
}

// LinkOutcome is what one write did.
type LinkOutcome int

const (
	Linked LinkOutcome = iota
	// NotLinkedChanged: the row is no longer an unlinked one of the
	// address.
	NotLinkedChanged
	// NotLinkedDuplicate: the account took a Ticket on the Event since.
	NotLinkedDuplicate
	// NotLinkedInactive: the account stopped being active since.
	NotLinkedInactive
)

// Store is the database side (Postgres).
type Store interface {
	Enabled(ctx context.Context) (bool, error)
	SetEnabled(ctx context.Context, on bool) error
	GuestTickets(ctx context.Context, addresses []string) ([]GuestTicket, error)
	OwnerlessCertificates(ctx context.Context, addresses []string) ([]OwnerlessCertificate, error)
	Accounts(ctx context.Context, ids []uuid.UUID) (Accounts, error)
	LinkTicket(ctx context.Context, l Link, source ticket.LinkSource) (LinkOutcome, error)
	LinkCertificate(ctx context.Context, l Link, source ticket.LinkSource) (LinkOutcome, error)
	OwnerFree(ctx context.Context, owner, eventID uuid.UUID) (bool, error)
	// CountUnlinked counts the unlinked guest Tickets with an address and
	// the ownerless certificates with one.
	CountUnlinked(ctx context.Context) (tickets, certificates int, err error)
}

// Run plans the links of every unlinked guest Ticket and ownerless
// certificate whose address addrs holds, and writes them when apply. A dry
// run writes nothing and counts what a run would do now. Each row is
// written on its own, guarded by what it was read as: a run stopped half
// way, or run twice, links each row once.
func Run(ctx context.Context, store Store, addrs map[string]commpref.VerifiedAddress, source ticket.LinkSource, apply bool) (Report, error) {
	addresses := make([]string, 0, len(addrs))
	var holders []uuid.UUID
	seen := map[uuid.UUID]bool{}
	for address, v := range addrs {
		addresses = append(addresses, address)
		for _, id := range v.Holders {
			if !seen[id] {
				seen[id] = true
				holders = append(holders, id)
			}
		}
	}
	slices.Sort(addresses)
	tickets, err := store.GuestTickets(ctx, addresses)
	if err != nil {
		return Report{}, err
	}
	certificates, err := store.OwnerlessCertificates(ctx, addresses)
	if err != nil {
		return Report{}, err
	}
	for _, c := range certificates {
		if c.TicketOwner != nil && !seen[*c.TicketOwner] {
			seen[*c.TicketOwner] = true
			holders = append(holders, *c.TicketOwner)
		}
	}
	accounts, err := store.Accounts(ctx, holders)
	if err != nil {
		return Report{}, err
	}
	report := Report{}
	var ticketLinks []Link
	ticketLinks, report.Tickets = PlanTickets(tickets, addrs, accounts)
	unlinkedTickets, unlinkedCertificates, err := store.CountUnlinked(ctx)
	if err != nil {
		return Report{}, err
	}
	// The rows read are those of an address the directory holds; the rest
	// are no account's.
	report.Tickets.Unmatched += max(unlinkedTickets-len(tickets), 0)
	linkedTickets := map[uuid.UUID]uuid.UUID{}
	if apply {
		for _, l := range ticketLinks {
			outcome, err := store.LinkTicket(ctx, l, source)
			if err != nil {
				return report, err
			}
			if outcome == Linked {
				linkedTickets[l.ID] = l.Owner
				continue
			}
			report.Tickets.Linked[l.Kind]--
			count(&report.Tickets, outcome)
		}
	} else {
		for _, l := range ticketLinks {
			linkedTickets[l.ID] = l.Owner
		}
	}
	certificateLinks, certificateCounts := PlanCertificates(certificates, addrs, accounts, linkedTickets)
	report.Certificates = certificateCounts
	report.Certificates.Unmatched += max(unlinkedCertificates-len(certificates), 0)
	if apply {
		for _, l := range certificateLinks {
			outcome, err := store.LinkCertificate(ctx, l, source)
			if err != nil {
				return report, err
			}
			if outcome != Linked {
				report.Certificates.Linked[l.Kind]--
				count(&report.Certificates, outcome)
			}
		}
	}
	return report, nil
}

func count(c *Counts, outcome LinkOutcome) {
	switch outcome {
	case NotLinkedDuplicate:
		c.Duplicate++
	case NotLinkedInactive:
		c.Inactive++
	default:
		c.Changed++
	}
}

// Directory is the verified-address directory (commpref.Recipients).
type Directory interface {
	VerifiedAddressOf(ctx context.Context, address string) (commpref.VerifiedAddress, bool, error)
	FreshVerifiedAddresses(ctx context.Context) (map[string]commpref.VerifiedAddress, error)
}

// switchTTL is how long the ticket service's writes take the switch as
// read: turning linking off takes at most this long to reach every
// replica's writes.
const switchTTL = 30 * time.Second

// lookupTimeout bounds the directory read a write may wait for: a cold or
// unanswering directory writes the Ticket unlinked rather than slowly.
const lookupTimeout = 2 * time.Second

// Resolver is the ticket service's GuestOwners: the account a new guest
// Ticket's address is, from the directory as the recipient question keeps
// it (at most 5 minutes old).
type Resolver struct {
	store Store
	dir   Directory
	logf  func(string, ...any)
	now   func() time.Time

	mu     sync.Mutex
	readAt time.Time
	on     bool
}

// NewResolver builds the resolver.
func NewResolver(store Store, dir Directory, logf func(string, ...any)) *Resolver {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Resolver{store: store, dir: dir, logf: logf, now: time.Now}
}

func (r *Resolver) enabled(ctx context.Context) bool {
	now := r.now()
	r.mu.Lock()
	if !r.readAt.IsZero() && now.Sub(r.readAt) < switchTTL {
		on := r.on
		r.mu.Unlock()
		return on
	}
	r.mu.Unlock()
	on, err := r.store.Enabled(ctx)
	if err != nil {
		r.logf(`{"event":"guest_account_link_failed","level":"warn","stage":"switch","error":%q}`, err.Error())
		return false
	}
	r.mu.Lock()
	r.on, r.readAt = on, now
	r.mu.Unlock()
	return on
}

// GuestOwner answers the account email (trimmed, lower case) is, when
// linking is on, one enabled account holds it verified, the account is
// active, and it holds no Ticket on the Event. Any failure is no owner:
// the Ticket is written unlinked and a later pass links it.
func (r *Resolver) GuestOwner(ctx context.Context, eventID uuid.UUID, email string) (uuid.UUID, bool) {
	if r == nil || r.dir == nil || !r.enabled(ctx) {
		return uuid.Nil, false
	}
	lookupCtx, cancel := context.WithTimeout(ctx, lookupTimeout)
	v, ok, err := r.dir.VerifiedAddressOf(lookupCtx, strings.ToLower(strings.TrimSpace(email)))
	cancel()
	if err != nil {
		r.logf(`{"event":"guest_account_link_failed","level":"warn","stage":"directory","error":%q}`, err.Error())
		return uuid.Nil, false
	}
	if !ok {
		return uuid.Nil, false
	}
	id, ok := v.Owner()
	if !ok {
		return uuid.Nil, false
	}
	free, err := r.store.OwnerFree(ctx, id, eventID)
	if err != nil {
		r.logf(`{"event":"guest_account_link_failed","level":"warn","stage":"database","error":%q}`, err.Error())
		return uuid.Nil, false
	}
	return id, free
}

// Linker is the user sync's last step: while linking is on, it reads the
// directory anew and links what the accounts' addresses now name.
type Linker struct {
	store Store
	dir   Directory
	logf  func(string, ...any)
}

// NewLinker builds the linker.
func NewLinker(store Store, dir Directory, logf func(string, ...any)) *Linker {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Linker{store: store, dir: dir, logf: logf}
}

// LinkGuests runs one pass. A failure is logged (no person) and left to
// the next pass.
func (l *Linker) LinkGuests(ctx context.Context) {
	on, err := l.store.Enabled(ctx)
	if err != nil {
		l.logf(`{"event":"guest_account_link_failed","level":"warn","stage":"switch","error":%q}`, err.Error())
		return
	}
	if !on {
		return
	}
	addrs, err := l.dir.FreshVerifiedAddresses(ctx)
	if err != nil {
		l.logf(`{"event":"guest_account_link_failed","level":"warn","stage":"directory","error":%q}`, err.Error())
		return
	}
	report, err := Run(ctx, l.store, addrs, ticket.LinkSync, true)
	if err != nil {
		l.logf(`{"event":"guest_account_link_failed","level":"warn","stage":"database","error":%q}`, err.Error())
	}
	if report.changed() || report.Tickets.Changed+report.Certificates.Changed > 0 {
		l.logf("%s", report.line("guest_account_link", ticket.LinkSync))
	}
}
