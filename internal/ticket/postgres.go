package ticket

import (
	"context"
	"errors"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/subjectlock"
)

type PostgresStore struct {
	pool *pgxpool.Pool
	// newAccessKey and newCode draw a new Ticket's access key and code;
	// tests replace them to force a taken one.
	newAccessKey func() (string, error)
	newCode      func() (string, error)
	// notifyCreated queues each new Ticket's Notification
	// (ticket_notifications, docs/ticket-created-notification.md) in the
	// statement that writes the Ticket. Set at startup, read by every
	// request: atomic.
	notifyCreated atomic.Bool
	// notifyStatus queues the ticket status mails (TICKET_STATUS_MAIL,
	// NotifyStatus): "Bekleme listesindesin" and "Yer açıldı".
	notifyStatus atomic.Bool
	// mailWelcome and mailRecorded queue the door's mails
	// (ATTENDANCE_MAIL, NotifyAttendance; docs/attendance-mail.md) in the
	// statement that writes the day's Venue entry and the Check-in.
	mailWelcome  atomic.Bool
	mailRecorded atomic.Bool
}

// AttendanceMailDelay is how long after the scan a door's mail is due
// (docs/attendance-mail.md): the welcome and the Session Check-ins of the
// same two minutes go as one mail, and an undo within them sends nothing.
const AttendanceMailDelay = 2 * time.Minute

// NotifyAttendance turns the door's mails on or off: while welcome is on,
// the statement that writes a Ticket's first Venue entry of a day queues its
// "hoş geldin" (attendance_mails); while recorded is on, the one that writes
// a Check-in queues its "katılımın alındı". Each only when the Event's own
// switch is on.
func (s *PostgresStore) NotifyAttendance(welcome, recorded bool) {
	s.mailWelcome.Store(welcome)
	s.mailRecorded.Store(recorded)
}

// queueWelcomeSQL queues the welcome of the Venue entry the CTE entry wrote
// (columns ticket_id, event_id, entry_date) while $welcome is true and the
// Event's switch is on. One a Ticket a day: a row already there is left,
// except one dropped (its entry was undone before it went), which the new
// entry queues again.
func queueWelcomeSQL(welcome string) string {
	return `
		INSERT INTO attendance_mails (id, kind, ticket_id, event_id, entry_date, next_attempt_at)
		SELECT gen_random_uuid(), 'welcome', entry.ticket_id, entry.event_id, entry.entry_date,
			now() + ` + attendanceDelaySQL + `
		FROM entry JOIN events e ON e.id = entry.event_id
		WHERE ` + welcome + `::boolean AND e.attendance_welcome_mail
		ON CONFLICT (ticket_id, entry_date) WHERE kind = 'welcome' DO UPDATE SET ` + requeueSQL
}

// attendanceDelaySQL is AttendanceMailDelay as an interval.
var attendanceDelaySQL = `interval '` + strconv.Itoa(int(AttendanceMailDelay/time.Second)) + ` seconds'`

// requeueSQL arms a dropped row again for a new scan.
const requeueSQL = `done_at = NULL, outcome = NULL, attempts = 0, claimed_at = NULL, attempted_at = NULL,
			created_at = now(), next_attempt_at = EXCLUDED.next_attempt_at
		WHERE attendance_mails.outcome = 'dropped'`

// NotifyCreated turns the new Ticket's Notification on or off: while on,
// Create also queues the Ticket in ticket_notifications, in the same
// statement, for the worker (internal/ticketnotify) to send.
func (s *PostgresStore) NotifyCreated(on bool) { s.notifyCreated.Store(on) }

// NotifiesCreated reports whether a Ticket written now queues its
// Notification (NotifyCreated).
func (s *PostgresStore) NotifiesCreated() bool { return s.notifyCreated.Load() }

func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool, newAccessKey: NewAccessKey, newCode: NewCode}
}

const ticketCols = `id, event_id, ticket_type, owner_id, guest_first_name, guest_last_name, guest_email, guest_phone_number, access_key, code, qr_version, created_at, updated_at,
	status, status_changed_at, waitlisted_at, cancelled_at, cancelled_by, cancel_reason, forms_rejected_at,
	form_response_id, form_response_status, guest_university, guest_department, guest_grade`

// doorTicketCols carry the Ticket's status: the door finds a cancelled
// Ticket to say so (409 ticket_cancelled), its lists leave it out.
const doorTicketCols = `t.id, t.event_id, t.ticket_type, t.owner_id, t.status`

const doorNameSQL = `CASE
	WHEN t.ticket_type = 'GUEST' THEN COALESCE(
		NULLIF(trim(concat_ws(' ', t.guest_first_name, t.guest_last_name)), ''),
		NULLIF(trim(t.guest_email), ''),
		''
	)
	ELSE COALESCE(
		NULLIF(trim(concat_ws(' ', u.first_name, u.last_name)), ''),
		NULLIF(trim(u.email), ''),
		t.owner_id::text,
		''
	)
END`

const doorEmailSQL = `CASE
	WHEN t.ticket_type = 'GUEST' THEN t.guest_email
	ELSE COALESCE(u.email, '')
END`

// createAttempts is how many codes Create draws before it gives up: with 40
// random bits, a second draw is already rare.
const createAttempts = 8

// Create writes a new Ticket with a fresh access key and code (and QR
// version 1). A code or key another Ticket holds is drawn again.
func (s *PostgresStore) Create(ctx context.Context, t Ticket) (Ticket, error) {
	if t.ID == uuid.Nil {
		t.ID = uuid.New()
	}
	owner, source, at := linkedInsert("$4", "$14")
	insert := `
		INSERT INTO tickets (
			id, event_id, ticket_type, owner_id,
			guest_first_name, guest_last_name, guest_email, guest_phone_number,
			access_key, code, guest_university, guest_department, guest_grade,
			owner_link_source, owner_linked_at
		) VALUES ($1,$2,$3,` + owner + `,$5,$6,$7,$8,$9,$10,$11,$12,$13,` + source + `,` + at + `)
		RETURNING ` + ticketCols
	if s.notifyCreated.Load() {
		// One statement writes the Ticket and queues its Notification:
		// neither is there without the other.
		insert = `WITH created AS (` + insert + `), queued AS (
			INSERT INTO ticket_notifications (ticket_id) SELECT id FROM created
		) SELECT ` + ticketCols + ` FROM created`
	}
	for attempt := 1; ; attempt++ {
		accessKey, err := s.newAccessKey()
		if err != nil {
			return Ticket{}, err
		}
		code, err := s.newCode()
		if err != nil {
			return Ticket{}, err
		}
		row := s.pool.QueryRow(ctx, insert,
			t.ID, t.EventID, t.TicketType, t.OwnerID,
			t.GuestFirstName, t.GuestLastName, t.GuestEmail, t.GuestPhoneNumber,
			accessKey, storedCode(code), t.GuestUniversity, t.GuestDepartment, t.GuestGrade, t.linkParam())
		got, err := scanTicket(row)
		if subjectlock.IsInactiveAccountReference(err) {
			return Ticket{}, ErrNotFound
		}
		if drawnTaken(err) && attempt < createAttempts {
			continue
		}
		if isUnique(err) && !drawnTaken(err) {
			// The Event already has a Ticket for the owner or the guest
			// e-mail, written since the caller looked.
			return Ticket{}, ErrConflict
		}
		if err != nil {
			return Ticket{}, err
		}
		got.CheckIns = []CheckIn{}
		return got, nil
	}
}

// drawnTaken reports whether err is the unique index of a Ticket's code or
// access key: the value drawn is another Ticket's.
func drawnTaken(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505" &&
		(pg.ConstraintName == "tickets_code_idx" || pg.ConstraintName == "tickets_access_key_idx")
}

func (s *PostgresStore) GetByAccessKey(ctx context.Context, key string) (Ticket, error) {
	if !ValidAccessKey(key) {
		return Ticket{}, ErrNotFound
	}
	t, err := scanTicket(s.pool.QueryRow(ctx, `SELECT `+ticketCols+` FROM tickets WHERE access_key = $1`, key))
	if errors.Is(err, pgx.ErrNoRows) {
		return Ticket{}, ErrNotFound
	}
	if err != nil {
		return Ticket{}, err
	}
	t.CheckIns = []CheckIn{}
	return t, nil
}

func (s *PostgresStore) Get(ctx context.Context, id uuid.UUID) (Ticket, error) {
	t, err := scanTicket(s.pool.QueryRow(ctx, `SELECT `+ticketCols+` FROM tickets WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Ticket{}, ErrNotFound
	}
	if err != nil {
		return Ticket{}, err
	}
	checkIns, err := s.checkInsFor(ctx, t.ID)
	if err != nil {
		return Ticket{}, err
	}
	t.CheckIns = checkIns
	return t, nil
}

func (s *PostgresStore) ListByOwner(ctx context.Context, ownerID uuid.UUID) ([]Ticket, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+ticketCols+` FROM tickets WHERE owner_id = $1 ORDER BY created_at`, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Ticket, 0)
	for rows.Next() {
		t, err := scanTicket(rows)
		if err != nil {
			return nil, err
		}
		checkIns, err := s.checkInsFor(ctx, t.ID)
		if err != nil {
			return nil, err
		}
		t.CheckIns = checkIns
		out = append(out, t)
	}
	return out, rows.Err()
}

// ListByEvent is the Event's active Tickets: every reader that means "the
// Event's Tickets" (mails, certificates, the door's fallback, counts) reads
// these. ListByEventStatus reads the others.
func (s *PostgresStore) ListByEvent(ctx context.Context, eventID uuid.UUID) ([]Ticket, error) {
	return s.ListByEventStatus(ctx, eventID, []Status{StatusActive})
}

// ListByEventStatus is the Event's Tickets of the statuses given (every
// status for none), oldest first.
func (s *PostgresStore) ListByEventStatus(ctx context.Context, eventID uuid.UUID, statuses []Status) ([]Ticket, error) {
	names := make([]string, 0, len(statuses))
	for _, status := range statuses {
		names = append(names, string(status))
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+ticketCols+` FROM tickets
		WHERE event_id = $1 AND (cardinality($2::text[]) = 0 OR status = ANY($2))
		ORDER BY created_at`, eventID, names)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Ticket, 0)
	for rows.Next() {
		t, err := scanTicket(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	return s.withCheckIns(ctx, out)
}

func (s *PostgresStore) withCheckIns(ctx context.Context, tickets []Ticket) ([]Ticket, error) {
	if len(tickets) == 0 {
		return tickets, nil
	}
	ids := make([]uuid.UUID, len(tickets))
	byID := make(map[uuid.UUID]int, len(tickets))
	for i := range tickets {
		ids[i] = tickets[i].ID
		byID[tickets[i].ID] = i
		tickets[i].CheckIns = []CheckIn{}
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, ticket_id, event_day_id, session_id, created_at
		FROM ticket_checkins
		WHERE ticket_id = ANY($1)
		ORDER BY created_at
	`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var c CheckIn
		if err := rows.Scan(&c.ID, &c.TicketID, &c.EventDayID, &c.SessionID, &c.CreatedAt); err != nil {
			return nil, err
		}
		if index, ok := byID[c.TicketID]; ok {
			tickets[index].CheckIns = append(tickets[index].CheckIns, c)
		}
	}
	return tickets, rows.Err()
}

func (s *PostgresStore) SearchDoorTickets(
	ctx context.Context,
	eventID uuid.UUID,
	query string,
	exact bool,
	limit int,
) ([]DoorTicketIdentity, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+doorTicketCols+`, `+doorNameSQL+`, `+doorEmailSQL+`,
		       (t.ticket_type = 'GUEST' OR u.id IS NOT NULL)
		FROM tickets t
		LEFT JOIN users u ON u.id = t.owner_id
		WHERE t.event_id = $1
		  AND (t.ticket_type = 'GUEST' OR u.id IS NOT NULL)
		  AND CASE WHEN $3 THEN (
			lower(trim(`+doorNameSQL+`)) = lower(trim($2))
			OR lower(trim(`+doorEmailSQL+`)) = lower(trim($2))
			OR t.owner_id::text = trim($2)
		  ) ELSE (
			lower(`+doorNameSQL+`) LIKE '%' || lower(trim($2)) || '%'
			OR lower(`+doorEmailSQL+`) LIKE '%' || lower(trim($2)) || '%'
			OR t.owner_id::text LIKE '%' || trim($2) || '%'
		  ) END
		ORDER BY lower(`+doorNameSQL+`), lower(`+doorEmailSQL+`), t.created_at
		LIMIT $4
	`, eventID, query, exact, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDoorTicketIdentities(rows)
}

func (s *PostgresStore) DoorTicketsByOwners(
	ctx context.Context,
	eventID uuid.UUID,
	ownerIDs []uuid.UUID,
) ([]DoorTicketIdentity, error) {
	if len(ownerIDs) == 0 {
		return []DoorTicketIdentity{}, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+doorTicketCols+`, `+doorNameSQL+`, `+doorEmailSQL+`, (u.id IS NOT NULL)
		FROM tickets t
		LEFT JOIN users u ON u.id = t.owner_id
		WHERE t.event_id = $1 AND t.owner_id = ANY($2)
		ORDER BY t.created_at
	`, eventID, ownerIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDoorTicketIdentities(rows)
}

func (s *PostgresStore) DoorSessionActivity(
	ctx context.Context,
	sessionID uuid.UUID,
	limit int,
) (DoorActivity, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.pool.Query(ctx, `
		SELECT c.id, c.ticket_id, c.event_day_id, c.session_id, c.created_at,
		       `+doorNameSQL+`, count(*) OVER()
		FROM ticket_checkins c
		JOIN tickets t ON t.id = c.ticket_id
		LEFT JOIN users u ON u.id = t.owner_id
		WHERE c.session_id = $1
		ORDER BY c.created_at DESC
		LIMIT $2
	`, sessionID, limit)
	if err != nil {
		return DoorActivity{}, err
	}
	defer rows.Close()
	activity := DoorActivity{Items: []DoorCheckIn{}}
	for rows.Next() {
		item := DoorCheckIn{Kind: KindCheckIn}
		if err := rows.Scan(
			&item.ID, &item.TicketID, &item.EventDayID, &item.SessionID, &item.CreatedAt,
			&item.PersonName, &activity.Total,
		); err != nil {
			return DoorActivity{}, err
		}
		activity.Items = append(activity.Items, item)
	}
	return activity, rows.Err()
}

// DoorTicketByCode is the Ticket of the Event with code (stored, without
// the hyphen) whose holder is still there: a guest with an address, a member
// with a core row.
func (s *PostgresStore) DoorTicketByCode(ctx context.Context, eventID uuid.UUID, code string) (DoorTicketIdentity, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+doorTicketCols+`, `+doorNameSQL+`, `+doorEmailSQL+`, (u.id IS NOT NULL OR t.ticket_type = 'GUEST')
		FROM tickets t
		LEFT JOIN users u ON u.id = t.owner_id
		WHERE t.event_id = $1 AND t.code = $2
		  AND ((t.ticket_type = 'GUEST' AND btrim(t.guest_email) <> '') OR u.id IS NOT NULL)
	`, eventID, code)
	if err != nil {
		return DoorTicketIdentity{}, err
	}
	defer rows.Close()
	found, err := scanDoorTicketIdentities(rows)
	if err != nil {
		return DoorTicketIdentity{}, err
	}
	if len(found) == 0 {
		return DoorTicketIdentity{}, ErrNotFound
	}
	return found[0], nil
}

func scanDoorTicketIdentities(rows pgx.Rows) ([]DoorTicketIdentity, error) {
	out := make([]DoorTicketIdentity, 0)
	for rows.Next() {
		var row DoorTicketIdentity
		if err := rows.Scan(
			&row.Ticket.ID, &row.Ticket.EventID, &row.Ticket.TicketType, &row.Ticket.OwnerID, &row.Ticket.Status,
			&row.Name, &row.Email, &row.Found,
		); err != nil {
			return nil, err
		}
		row.Ticket.CheckIns = []CheckIn{}
		out = append(out, row)
	}
	return out, rows.Err()
}

func (s *PostgresStore) ExistsOwnerEvent(ctx context.Context, ownerID, eventID uuid.UUID) (bool, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT COUNT(1) FROM tickets WHERE owner_id = $1 AND event_id = $2
	`, ownerID, eventID).Scan(&n)
	return n > 0, err
}

func (s *PostgresStore) ExistsGuestEvent(ctx context.Context, email string, eventID uuid.UUID) (bool, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT COUNT(1) FROM tickets WHERE lower(guest_email) = lower($1) AND event_id = $2
	`, email, eventID).Scan(&n)
	return n > 0, err
}

func (s *PostgresStore) GetByGuestEvent(ctx context.Context, email string, eventID uuid.UUID) (Ticket, error) {
	t, err := scanTicket(s.pool.QueryRow(ctx, `
		SELECT `+ticketCols+` FROM tickets
		WHERE ticket_type = 'GUEST' AND lower(guest_email) = lower($1) AND event_id = $2
	`, email, eventID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Ticket{}, ErrNotFound
	}
	if err != nil {
		return Ticket{}, err
	}
	checkIns, err := s.checkInsFor(ctx, t.ID)
	if err != nil {
		return Ticket{}, err
	}
	t.CheckIns = checkIns
	return t, nil
}

func (s *PostgresStore) Update(ctx context.Context, t Ticket) (Ticket, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE tickets SET
			guest_first_name = $1,
			guest_last_name = $2,
			guest_email = $3,
			guest_phone_number = $4,
			guest_university = $6,
			guest_department = $7,
			guest_grade = $8,
			updated_at = now()
		WHERE id = $5
		RETURNING `+ticketCols,
		t.GuestFirstName, t.GuestLastName, t.GuestEmail, t.GuestPhoneNumber, t.ID,
		t.GuestUniversity, t.GuestDepartment, t.GuestGrade)
	got, err := scanTicket(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Ticket{}, ErrNotFound
	}
	if err != nil {
		return Ticket{}, err
	}
	checkIns, err := s.checkInsFor(ctx, got.ID)
	if err != nil {
		return Ticket{}, err
	}
	got.CheckIns = checkIns
	return got, nil
}

func (s *PostgresStore) GetByOwnerEvent(ctx context.Context, ownerID, eventID uuid.UUID) (Ticket, error) {
	t, err := scanTicket(s.pool.QueryRow(ctx, `SELECT `+ticketCols+` FROM tickets WHERE owner_id = $1 AND event_id = $2`, ownerID, eventID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Ticket{}, ErrNotFound
	}
	if err != nil {
		return Ticket{}, err
	}
	checkIns, err := s.checkInsFor(ctx, t.ID)
	if err != nil {
		return Ticket{}, err
	}
	t.CheckIns = checkIns
	return t, nil
}

func (s *PostgresStore) ListByGuestEmail(ctx context.Context, email string) ([]Ticket, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+ticketCols+` FROM tickets WHERE lower(guest_email) = lower($1) ORDER BY created_at`, email)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Ticket, 0)
	for rows.Next() {
		t, err := scanTicket(rows)
		if err != nil {
			return nil, err
		}
		checkIns, err := s.checkInsFor(ctx, t.ID)
		if err != nil {
			return nil, err
		}
		t.CheckIns = checkIns
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *PostgresStore) AddCheckIn(ctx context.Context, c CheckIn) (CheckIn, error) {
	if c.ID == uuid.Nil {
		c.ID = uuid.New()
	}
	entry := c.Entry
	c.Entry, c.EntryWritten = nil, false
	var date, source *string
	var by *uuid.UUID
	if entry != nil {
		date, source, by = &entry.EntryDate, (*string)(&entry.Source), entry.ByUserID
	}
	// Only an active Ticket takes a check-in: a cancellation committed
	// since the caller read the Ticket is refused here too. The day's
	// Venue entry is written in the same statement when the Ticket has
	// none that day (docs/venue-entry.md): a door scan's names no
	// Check-in (undoing the Check-in keeps it), the others' name it.
	err := s.pool.QueryRow(ctx, `
		WITH ci AS (
			INSERT INTO ticket_checkins (id, ticket_id, event_day_id, session_id, scanned_at, source, by_user_id)
			SELECT $1, t.id, $3, $4, $10, NULLIF($11, ''), `+activeActorSQL("$12")+` FROM tickets t WHERE t.id = $2 AND t.status = 'active'
			RETURNING id, ticket_id, event_day_id, session_id, created_at, scanned_at
		), entry AS (
			INSERT INTO venue_entries (id, ticket_id, event_id, entry_date, event_day_id, source, check_in_id, by_user_id, scanned_at)
			SELECT gen_random_uuid(), ci.ticket_id, t.event_id, $5::date, ci.event_day_id, $6,
				CASE WHEN $6 <> 'scan' THEN ci.id END, `+activeActorSQL("$7")+`, ci.scanned_at
			FROM ci JOIN tickets t ON t.id = ci.ticket_id
			WHERE $5::date IS NOT NULL
			ON CONFLICT (ticket_id, entry_date) DO NOTHING
			RETURNING id, ticket_id, event_id, entry_date
		), welcome AS (`+queueWelcomeSQL("$8")+`
		), recorded AS (
			-- The Check-in's receipt (docs/attendance-mail.md): one a Ticket a
			-- Session, on the entry's day (the scan's Istanbul day without one).
			INSERT INTO attendance_mails (id, kind, ticket_id, event_id, entry_date, session_id, next_attempt_at)
			SELECT gen_random_uuid(), 'recorded', ci.ticket_id, t.event_id,
				COALESCE($5::date, (now() AT TIME ZONE 'Europe/Istanbul')::date), ci.session_id,
				now() + `+attendanceDelaySQL+`
			FROM ci JOIN tickets t ON t.id = ci.ticket_id JOIN events e ON e.id = t.event_id
			WHERE $9::boolean AND e.attendance_recorded_mail
			ON CONFLICT (ticket_id, session_id) WHERE kind = 'recorded' DO UPDATE SET `+requeueSQL+`
		)
		SELECT ci.id, ci.ticket_id, ci.event_day_id, ci.session_id, ci.created_at, ci.scanned_at, EXISTS (SELECT 1 FROM entry)
		FROM ci
	`, c.ID, c.TicketID, c.EventDayID, c.SessionID, date, source, by, s.mailWelcome.Load() && !c.NoMail,
		s.mailRecorded.Load() && !c.NoMail, c.ScannedAt, string(c.Source), c.ByUserID).Scan(
		&c.ID, &c.TicketID, &c.EventDayID, &c.SessionID, &c.CreatedAt, &c.ScannedAt, &c.EntryWritten)
	c.NoMail = false
	if isUnique(err) {
		return CheckIn{}, ErrAlreadyCheckedIn
	}
	if errors.Is(err, pgx.ErrNoRows) {
		status, serr := s.statusOf(ctx, c.TicketID)
		if serr != nil {
			return CheckIn{}, serr
		}
		if refused := statusErr(status); refused != nil {
			return CheckIn{}, refused
		}
		return CheckIn{}, ErrConflict
	}
	return c, err
}

func isUnique(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}

// RemoveCheckIn takes back the one row (ticket_id, session_id) is unique
// on. Nothing on the tickets row is derived from its check-ins: every
// reader counts ticket_checkins itself.
func (s *PostgresStore) RemoveCheckIn(ctx context.Context, ticketID, sessionID uuid.UUID, undoneBy *uuid.UUID) (bool, error) {
	return s.removeCheckIn(ctx, undoneBy, `ticket_id = $1 AND session_id = $2`, ticketID, sessionID)
}

// removeCheckIn takes back the check-in where names and, in the same
// transaction, the Venue entry it wrote (docs/venue-entry.md): taken back
// when the Ticket has no other check-in that day, else handed to that one,
// which holds it now. A scanned entry names no check-in and stays. What is
// taken back moves to door_undos with who took it back (Deletion
// lifecycle): the live tables keep what stands. The entry is settled
// before the check-in goes: its foreign key would clear check_in_id first.
func (s *PostgresStore) removeCheckIn(ctx context.Context, undoneBy *uuid.UUID, where string, args ...any) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var id, ticketID, dayID uuid.UUID
	err = tx.QueryRow(ctx, `SELECT id, ticket_id, event_day_id FROM ticket_checkins WHERE `+where+` FOR UPDATE`, args...).
		Scan(&id, &ticketID, &dayID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `
		WITH other AS (
			SELECT c.id FROM ticket_checkins c
			WHERE c.ticket_id = $2 AND c.event_day_id = $3 AND c.id <> $1
			ORDER BY c.created_at LIMIT 1
		), moved AS (
			UPDATE venue_entries SET check_in_id = (SELECT id FROM other)
			WHERE check_in_id = $1 AND EXISTS (SELECT 1 FROM other)
			RETURNING id
		), gone AS (
			DELETE FROM venue_entries
			WHERE check_in_id = $1 AND source IN ('attendance', 'desk') AND NOT EXISTS (SELECT 1 FROM other)
			RETURNING *
		)
		`+undoEntrySQL("$4")+`
	`, id, ticketID, dayID, undoneBy); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `
		WITH gone AS (
			DELETE FROM ticket_checkins WHERE id = $1
			RETURNING *
		)
		INSERT INTO door_undos (id, kind, ticket_id, event_id, session_id, event_day_id, source, by_user_id, written_at, scanned_at, undone_by)
		SELECT g.id, 'check_in', g.ticket_id, t.event_id, g.session_id, g.event_day_id, g.source,
			`+activeActorSQL("g.by_user_id")+`, g.created_at, g.scanned_at, `+activeActorSQL("$2")+`
		FROM gone g JOIN tickets t ON t.id = g.ticket_id
	`, id, undoneBy); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// undoEntrySQL keeps the Venue entries a CTE named gone deleted as door
// undos by undoneBy (a parameter): the row as it was, its writer while
// still an active account.
func undoEntrySQL(undoneBy string) string {
	return `INSERT INTO door_undos (id, kind, ticket_id, event_id, event_day_id, entry_date, source, by_user_id, written_at, scanned_at, undone_by)
		SELECT g.id, 'entry', g.ticket_id, g.event_id, g.event_day_id, g.entry_date, g.source,
			` + activeActorSQL("g.by_user_id") + `, g.created_at, g.scanned_at, ` + activeActorSQL(undoneBy) + `
		FROM gone g`
}

func (s *PostgresStore) CheckInByID(ctx context.Context, id uuid.UUID) (CheckIn, error) {
	var c CheckIn
	err := s.pool.QueryRow(ctx, `
		SELECT id, ticket_id, event_day_id, session_id, created_at
		FROM ticket_checkins WHERE id = $1
	`, id).Scan(&c.ID, &c.TicketID, &c.EventDayID, &c.SessionID, &c.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return CheckIn{}, ErrNotFound
	}
	return c, err
}

func (s *PostgresStore) RemoveCheckInByID(ctx context.Context, id uuid.UUID, undoneBy *uuid.UUID) (bool, error) {
	return s.removeCheckIn(ctx, undoneBy, `id = $1`, id)
}

// CheckInOf is the Ticket's Check-in at the Session (ScanRecords).
func (s *PostgresStore) CheckInOf(ctx context.Context, ticketID, sessionID uuid.UUID) (CheckIn, error) {
	c := CheckIn{}
	err := s.pool.QueryRow(ctx, `
		SELECT id, ticket_id, event_day_id, session_id, created_at, scanned_at
		FROM ticket_checkins WHERE ticket_id = $1 AND session_id = $2`, ticketID, sessionID).
		Scan(&c.ID, &c.TicketID, &c.EventDayID, &c.SessionID, &c.CreatedAt, &c.ScannedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return CheckIn{}, ErrNotFound
	}
	return c, err
}

func (s *PostgresStore) HasCheckIn(ctx context.Context, ticketID, sessionID uuid.UUID) (bool, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT COUNT(1) FROM ticket_checkins WHERE ticket_id = $1 AND session_id = $2
	`, ticketID, sessionID).Scan(&n)
	return n > 0, err
}

func (s *PostgresStore) checkInsFor(ctx context.Context, ticketID uuid.UUID) ([]CheckIn, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, ticket_id, event_day_id, session_id, created_at
		FROM ticket_checkins WHERE ticket_id = $1 ORDER BY created_at
	`, ticketID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]CheckIn, 0)
	for rows.Next() {
		var c CheckIn
		if err := rows.Scan(&c.ID, &c.TicketID, &c.EventDayID, &c.SessionID, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanTicket(row rowScanner) (Ticket, error) {
	var t Ticket
	var created, updated time.Time
	var code string
	var accessKey, cancelledBy, cancelReason *string
	var formResponse *uuid.UUID
	var formResponseStatus *string
	err := row.Scan(
		&t.ID, &t.EventID, &t.TicketType, &t.OwnerID,
		&t.GuestFirstName, &t.GuestLastName, &t.GuestEmail, &t.GuestPhoneNumber,
		&accessKey, &code, &t.QRVersion,
		&created, &updated,
		&t.Status, &t.StatusChangedAt, &t.WaitlistedAt, &t.CancelledAt, &cancelledBy, &cancelReason, &t.FormsRejectedAt,
		&formResponse, &formResponseStatus, &t.GuestUniversity, &t.GuestDepartment, &t.GuestGrade,
	)
	t.FormsStatus = FormsStatusOf(formResponse != nil, formResponseStatus, t.FormsRejectedAt)
	if accessKey != nil {
		t.AccessKey = *accessKey
	}
	if cancelledBy != nil {
		t.CancelledBy = CancelledBy(*cancelledBy)
	}
	if cancelReason != nil {
		t.CancelReason = *cancelReason
	}
	t.Code = FormatCode(code)
	t.CreatedAt = created
	t.UpdatedAt = updated
	return t, err
}
