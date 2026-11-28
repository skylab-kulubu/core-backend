package ticket

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// activeActorSQL is param (a door staff's id, or NULL) when it names an
// active account with no deletion marker, else NULL: venue_entries'
// by_user_id guard would refuse anyone else, and a door write must not
// fail on who wrote it.
func activeActorSQL(param string) string {
	return `(SELECT u.id FROM users u
		WHERE u.id = ` + param + `::uuid AND u.account_state = 'active'
		  AND NOT EXISTS (SELECT 1 FROM account_deletion_requests r WHERE r.subject_id = u.id))`
}

const venueEntryCols = `v.id, v.ticket_id, v.event_id, v.entry_date::text, v.event_day_id, v.source, v.check_in_id, v.by_user_id, v.created_at, v.scanned_at`

func scanVenueEntry(row rowScanner) (VenueEntry, error) {
	var e VenueEntry
	err := row.Scan(&e.ID, &e.TicketID, &e.EventID, &e.EntryDate, &e.EventDayID, &e.Source, &e.CheckInID, &e.ByUserID, &e.CreatedAt, &e.ScannedAt)
	return e, err
}

func (s *PostgresStore) AddVenueEntry(ctx context.Context, e VenueEntry) (VenueEntry, error) {
	if e.ID == uuid.Nil {
		e.ID = uuid.New()
	}
	// The day's welcome mail is queued in the same statement
	// (docs/attendance-mail.md).
	err := s.pool.QueryRow(ctx, `
		WITH entry AS (
			INSERT INTO venue_entries AS v (id, ticket_id, event_id, entry_date, event_day_id, source, check_in_id, by_user_id, scanned_at)
			SELECT $1, t.id, t.event_id, $3::date, $4, $5, $6, `+activeActorSQL("$7")+`, $9
			FROM tickets t WHERE t.id = $2 AND t.status = 'active'
			ON CONFLICT (ticket_id, entry_date) DO NOTHING
			RETURNING *
		), welcome AS (`+queueWelcomeSQL("$8")+`
		)
		SELECT `+venueEntryCols+` FROM entry v`,
		e.ID, e.TicketID, e.EntryDate, e.EventDayID, string(e.Source), e.CheckInID, e.ByUserID, s.mailWelcome.Load() && !e.NoMail, e.ScannedAt,
	).Scan(&e.ID, &e.TicketID, &e.EventID, &e.EntryDate, &e.EventDayID, &e.Source, &e.CheckInID, &e.ByUserID, &e.CreatedAt, &e.ScannedAt)
	e.NoMail = false
	if !errors.Is(err, pgx.ErrNoRows) {
		return e, err
	}
	// No row: the Ticket is not there, not active, or already in today.
	status, serr := s.statusOf(ctx, e.TicketID)
	if serr != nil {
		return VenueEntry{}, serr
	}
	if refused := statusErr(status); refused != nil {
		return VenueEntry{}, refused
	}
	return VenueEntry{}, &EntryConflictError{}
}

func (s *PostgresStore) VenueEntryByID(ctx context.Context, id uuid.UUID) (VenueEntry, error) {
	e, err := scanVenueEntry(s.pool.QueryRow(ctx, `SELECT `+venueEntryCols+` FROM venue_entries v WHERE v.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return VenueEntry{}, ErrNotFound
	}
	return e, err
}

// VenueEntryOn is the Ticket's Venue entry of the venue day (ScanRecords).
func (s *PostgresStore) VenueEntryOn(ctx context.Context, ticketID uuid.UUID, date string) (VenueEntry, error) {
	e, err := scanVenueEntry(s.pool.QueryRow(ctx, `SELECT `+venueEntryCols+` FROM venue_entries v WHERE v.ticket_id = $1 AND v.entry_date = $2::date`, ticketID, date))
	if errors.Is(err, pgx.ErrNoRows) {
		return VenueEntry{}, ErrNotFound
	}
	return e, err
}

// RemoveVenueEntry takes the entry out of venue_entries and keeps it in
// door_undos, in one statement (docs/venue-entry.md "Taking back").
func (s *PostgresStore) RemoveVenueEntry(ctx context.Context, id uuid.UUID, undoneBy *uuid.UUID) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		WITH gone AS (
			DELETE FROM venue_entries WHERE id = $1
			RETURNING *
		)
		`+undoEntrySQL("$2"), id, undoneBy)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

func (s *PostgresStore) VenueEntriesOn(ctx context.Context, eventID uuid.UUID, date string) ([]DoorEntry, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT v.id, v.ticket_id, v.entry_date::text, v.event_day_id, v.source, v.created_at, v.scanned_at, `+doorNameSQL+`
		FROM venue_entries v
		JOIN tickets t ON t.id = v.ticket_id
		LEFT JOIN users u ON u.id = t.owner_id
		WHERE v.event_id = $1 AND v.entry_date = $2::date
		ORDER BY v.created_at DESC
	`, eventID, date)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]DoorEntry, 0)
	for rows.Next() {
		e := DoorEntry{Kind: KindEntry}
		if err := rows.Scan(&e.ID, &e.TicketID, &e.EntryDate, &e.EventDayID, &e.Source, &e.CreatedAt, &e.ScannedAt, &e.Name); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *PostgresStore) VenueEntryDays(ctx context.Context, eventID uuid.UUID) ([]EntryDay, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT v.entry_date::text, (array_agg(v.event_day_id) FILTER (WHERE v.event_day_id IS NOT NULL))[1],
			count(*), count(*) FILTER (WHERE v.source = 'scan'), count(*) FILTER (WHERE v.source = 'desk'),
			count(*) FILTER (WHERE v.source = 'attendance'), count(*) FILTER (WHERE v.source = 'walk_in')
		FROM venue_entries v
		WHERE v.event_id = $1
		GROUP BY v.entry_date
		ORDER BY v.entry_date
	`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]EntryDay, 0)
	for rows.Next() {
		var day EntryDay
		var scan, desk, attendance, walkIn int
		if err := rows.Scan(&day.Date, &day.EventDayID, &day.Total, &scan, &desk, &attendance, &walkIn); err != nil {
			return nil, err
		}
		day.BySource = map[string]int{
			string(EntryScan): scan, string(EntryDesk): desk, string(EntryAttendance): attendance, string(EntryWalkIn): walkIn,
		}
		out = append(out, day)
	}
	return out, rows.Err()
}
