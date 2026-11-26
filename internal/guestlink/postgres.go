package guestlink

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/subjectlock"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

// Postgres is Store on core's database.
type Postgres struct {
	pool *pgxpool.Pool
}

// NewPostgres builds the store on pool.
func NewPostgres(pool *pgxpool.Pool) *Postgres {
	return &Postgres{pool: pool}
}

var _ Store = (*Postgres)(nil)

// Enabled reads the switch.
func (p *Postgres) Enabled(ctx context.Context) (bool, error) {
	var on bool
	err := p.pool.QueryRow(ctx, `SELECT enabled_at IS NOT NULL FROM guest_account_link_settings WHERE id`).Scan(&on)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return on, err
}

// SetEnabled turns linking on (keeping the first time it was turned on) or
// off.
func (p *Postgres) SetEnabled(ctx context.Context, on bool) error {
	_, err := p.pool.Exec(ctx, `
		INSERT INTO guest_account_link_settings (id, enabled_at) VALUES (true, CASE WHEN $1 THEN now() END)
		ON CONFLICT (id) DO UPDATE SET enabled_at = CASE WHEN $1 THEN COALESCE(guest_account_link_settings.enabled_at, now()) END`, on)
	return err
}

// guestAddressSQL is a guest Ticket's address as it is compared: trimmed
// and lower case (Guest apply stores it so; older rows may not be).
const guestAddressSQL = `lower(btrim(t.guest_email))`

// GuestTickets reads the unlinked guest Tickets whose address is one of
// addresses.
func (p *Postgres) GuestTickets(ctx context.Context, addresses []string) ([]GuestTicket, error) {
	if len(addresses) == 0 {
		return nil, nil
	}
	rows, err := p.pool.Query(ctx, `
		SELECT t.id, t.event_id, `+guestAddressSQL+`, t.status, t.created_at
		FROM tickets t
		WHERE t.owner_id IS NULL AND t.ticket_type = 'GUEST' AND btrim(t.guest_email) <> ''
		  AND `+guestAddressSQL+` = ANY($1)`, addresses)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GuestTicket
	for rows.Next() {
		var t GuestTicket
		if err := rows.Scan(&t.ID, &t.EventID, &t.Address, &t.Status, &t.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// OwnerlessCertificates reads the certificates without an owner that still
// have an address: those whose Ticket has an owner, and those whose
// address is one of addresses.
func (p *Postgres) OwnerlessCertificates(ctx context.Context, addresses []string) ([]OwnerlessCertificate, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT c.id, c.ticket_id, lower(btrim(c.recipient_email)), t.owner_id
		FROM certificates c
		JOIN tickets t ON t.id = c.ticket_id
		WHERE c.owner_id IS NULL AND btrim(c.recipient_email) <> ''
		  AND (t.owner_id IS NOT NULL OR lower(btrim(c.recipient_email)) = ANY($1))`, addresses)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OwnerlessCertificate
	for rows.Next() {
		var c OwnerlessCertificate
		if err := rows.Scan(&c.ID, &c.TicketID, &c.Address, &c.TicketOwner); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CountUnlinked counts the unlinked guest Tickets with an address and the
// ownerless certificates with one.
func (p *Postgres) CountUnlinked(ctx context.Context) (int, int, error) {
	var tickets, certificates int
	err := p.pool.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM tickets t WHERE t.owner_id IS NULL AND t.ticket_type = 'GUEST' AND btrim(t.guest_email) <> ''),
			(SELECT count(*) FROM certificates c WHERE c.owner_id IS NULL AND btrim(c.recipient_email) <> '')`).Scan(&tickets, &certificates)
	return tickets, certificates, err
}

// activeAccountSQL is the database's active_account_reference rule for $1:
// an active row and no erasure request.
const activeAccountSQL = `EXISTS (SELECT 1 FROM users au WHERE au.id = $2 AND au.account_state = 'active')
	AND NOT EXISTS (SELECT 1 FROM account_deletion_requests ad WHERE ad.subject_id = $2)`

// Accounts reads which of ids are active accounts and the Events they hold
// a Ticket on.
func (p *Postgres) Accounts(ctx context.Context, ids []uuid.UUID) (Accounts, error) {
	out := Accounts{Active: map[uuid.UUID]bool{}, Holds: map[[2]uuid.UUID]bool{}}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := p.pool.Query(ctx, `
		SELECT u.id FROM users u
		WHERE u.id = ANY($1) AND u.account_state = 'active'
		  AND NOT EXISTS (SELECT 1 FROM account_deletion_requests ad WHERE ad.subject_id = u.id)`, ids)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return out, err
		}
		out.Active[id] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}
	rows, err = p.pool.Query(ctx, `SELECT owner_id, event_id FROM tickets WHERE owner_id = ANY($1)`, ids)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var owner, eventID uuid.UUID
		if err := rows.Scan(&owner, &eventID); err != nil {
			return out, err
		}
		out.Holds[[2]uuid.UUID{owner, eventID}] = true
	}
	return out, rows.Err()
}

// OwnerFree reports whether owner is an active account holding no Ticket on
// the Event.
func (p *Postgres) OwnerFree(ctx context.Context, owner, eventID uuid.UUID) (bool, error) {
	var free bool
	err := p.pool.QueryRow(ctx, `
		SELECT `+activeAccountSQL+`
		  AND NOT EXISTS (SELECT 1 FROM tickets o WHERE o.owner_id = $2 AND o.event_id = $1)`, eventID, owner).Scan(&free)
	return free, err
}

// LinkTicket writes l's owner on the Ticket while it is still an unlinked
// guest Ticket of the address. The database's own rules decide the rest:
// one Ticket per account and Event (tickets_owner_event_idx), an active
// account (active_account_reference, which also serializes with an
// erasure request).
func (p *Postgres) LinkTicket(ctx context.Context, l Link, source ticket.LinkSource) (LinkOutcome, error) {
	tag, err := p.pool.Exec(ctx, `
		UPDATE tickets t SET owner_id = $2, owner_link_source = $3, owner_linked_at = now()
		WHERE t.id = $1 AND t.owner_id IS NULL AND t.ticket_type = 'GUEST' AND `+guestAddressSQL+` = $4`,
		l.ID, l.Owner, string(source), l.Address)
	switch {
	case subjectlock.IsInactiveAccountReference(err):
		return NotLinkedInactive, nil
	case isUnique(err):
		return NotLinkedDuplicate, nil
	case err != nil:
		return 0, err
	case tag.RowsAffected() == 0:
		return NotLinkedChanged, nil
	}
	return Linked, nil
}

// LinkCertificate writes l's owner on the certificate while it has none
// and still has its address, under the account's subject lock (the lock an
// erasure request takes) and only while the account is active: certificates
// have no trigger of their own.
func (p *Postgres) LinkCertificate(ctx context.Context, l Link, source ticket.LinkSource) (LinkOutcome, error) {
	outcome := Linked
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		if err := subjectlock.Lock(ctx, tx, l.Owner); err != nil {
			return err
		}
		var active bool
		if err := tx.QueryRow(ctx, `SELECT `+activeAccountSQL+` FROM (SELECT $1::uuid) x`, uuid.Nil, l.Owner).Scan(&active); err != nil {
			return err
		}
		if !active {
			outcome = NotLinkedInactive
			return nil
		}
		tag, err := tx.Exec(ctx, `
			UPDATE certificates SET owner_id = $2, owner_link_source = $3, owner_linked_at = now()
			WHERE id = $1 AND owner_id IS NULL AND btrim(recipient_email) <> ''`, l.ID, l.Owner, string(source))
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			outcome = NotLinkedChanged
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return outcome, nil
}

// Undone counts what Unlink took back.
type Undone struct {
	Tickets, Certificates, IssuedFromLinked int64
}

// Unlink turns linking off and takes back every link core made, in one
// transaction: the certificates issued from a linked Ticket after the link
// (they took its owner), the linked certificates, the linked Tickets. The
// down migration does the same.
func (p *Postgres) Unlink(ctx context.Context) (Undone, error) {
	var out Undone
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE guest_account_link_settings SET enabled_at = NULL`); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `
			UPDATE certificates c SET owner_id = NULL
			FROM tickets t
			WHERE c.ticket_id = t.id AND t.owner_link_source IS NOT NULL
			  AND c.owner_link_source IS NULL AND c.owner_id = t.owner_id`)
		if err != nil {
			return err
		}
		out.IssuedFromLinked = tag.RowsAffected()
		if tag, err = tx.Exec(ctx, `
			UPDATE certificates SET owner_id = NULL, owner_linked_at = NULL, owner_link_source = NULL
			WHERE owner_link_source IS NOT NULL`); err != nil {
			return err
		}
		out.Certificates = tag.RowsAffected()
		if tag, err = tx.Exec(ctx, `
			UPDATE tickets SET owner_id = NULL, owner_linked_at = NULL, owner_link_source = NULL
			WHERE owner_link_source IS NOT NULL`); err != nil {
			return err
		}
		out.Tickets = tag.RowsAffected()
		return nil
	})
	return out, err
}

// LinkedCounts counts the Tickets and certificates core linked, by source:
// what Unlink would take back.
func (p *Postgres) LinkedCounts(ctx context.Context) (map[string]int, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT 'tickets_' || owner_link_source, count(*) FROM tickets WHERE owner_link_source IS NOT NULL GROUP BY owner_link_source
		UNION ALL
		SELECT 'certificates_' || owner_link_source, count(*) FROM certificates WHERE owner_link_source IS NOT NULL GROUP BY owner_link_source`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var key string
		var n int
		if err := rows.Scan(&key, &n); err != nil {
			return nil, err
		}
		out[key] = n
	}
	return out, rows.Err()
}

func isUnique(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}
