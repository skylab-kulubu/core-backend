package event

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/skylab-kulubu/core-backend/internal/subjectlock"
)

// SetCoOrganizers replaces the Event's Co-organizer rows in one
// transaction, under the Event's row lock (docs/co-organizers.md).
func (s *PostgresStore) SetCoOrganizers(ctx context.Context, w CoOrganizersWrite) (Event, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Event{}, err
	}
	defer tx.Rollback(ctx)
	var owner string
	err = tx.QueryRow(ctx, `SELECT owner_team FROM events WHERE id = $1 AND archived_at IS NULL FOR UPDATE`, w.EventID).Scan(&owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return Event{}, ErrNotFound
	}
	if err != nil {
		return Event{}, err
	}
	if owner != w.OwnerTeam {
		return Event{}, ErrConflict
	}
	if _, err := tx.Exec(ctx, `DELETE FROM event_co_organizers WHERE event_id = $1`, w.EventID); err != nil {
		return Event{}, err
	}
	for _, team := range w.Teams {
		if _, err := tx.Exec(ctx, `
			INSERT INTO event_co_organizers (event_id, team_code, management, visibility, added_by)
			VALUES ($1, $2, $3, $4, $5)`, w.EventID, team.Code, string(team.Management), string(team.Visibility), w.By); err != nil {
			return Event{}, err
		}
	}
	for _, person := range w.People {
		if _, err := tx.Exec(ctx, `
			INSERT INTO event_co_organizers (event_id, user_id, added_by) VALUES ($1, $2, $3)`, w.EventID, person, w.By); err != nil {
			if subjectlock.IsInactiveAccountReference(err) {
				return Event{}, &InactivePersonError{ID: person}
			}
			return Event{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Event{}, err
	}
	return s.Get(ctx, w.EventID)
}

// CoOrganizerPeople reads the Event's Co-organizer people with their names
// and e-mails, by name.
func (s *PostgresStore) CoOrganizerPeople(ctx context.Context, eventID uuid.UUID) ([]OrganizerPerson, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT c.user_id, btrim(COALESCE(u.first_name, '') || ' ' || COALESCE(u.last_name, '')), COALESCE(u.email, '')
		FROM event_co_organizers c
		LEFT JOIN users u ON u.id = c.user_id
		WHERE c.event_id = $1 AND c.user_id IS NOT NULL
		ORDER BY 2, 1`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]OrganizerPerson, 0)
	for rows.Next() {
		var person OrganizerPerson
		if err := rows.Scan(&person.ID, &person.Name, &person.Email); err != nil {
			return nil, err
		}
		out = append(out, person)
	}
	return out, rows.Err()
}

// withCoOrganizers reads the Co-organizer teams and people of every Event,
// in one query whatever the number of Events: teams by code, people by id;
// nil lists for an Event with none.
func (s *PostgresStore) withCoOrganizers(ctx context.Context, ids []uuid.UUID, events []Event, byID map[uuid.UUID]int) error {
	rows, err := s.pool.Query(ctx, `
		SELECT event_id, team_code, user_id, management, visibility
		FROM event_co_organizers WHERE event_id = ANY($1)
		ORDER BY event_id, team_code NULLS LAST, user_id`, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var eventID uuid.UUID
		var code, management, visibility *string
		var userID *uuid.UUID
		if err := rows.Scan(&eventID, &code, &userID, &management, &visibility); err != nil {
			return err
		}
		e := &events[byID[eventID]]
		switch {
		case code != nil && management != nil && visibility != nil:
			e.CoOrganizers = append(e.CoOrganizers, CoOrganizerTeam{
				Code: *code, Management: Management(*management), Visibility: Visibility(*visibility),
			})
		case userID != nil:
			e.CoOrganizerIDs = append(e.CoOrganizerIDs, *userID)
		}
	}
	return rows.Err()
}

// dropOwnerCoOrganizer removes the Co-organizer row of the team that is now
// the Event's Owner team, in the Event's write.
func dropOwnerCoOrganizer(ctx context.Context, tx pgx.Tx, eventID uuid.UUID, ownerTeam string) error {
	if strings.TrimSpace(ownerTeam) == "" {
		return nil
	}
	_, err := tx.Exec(ctx, `DELETE FROM event_co_organizers WHERE event_id = $1 AND lower(team_code) = lower(btrim($2))`, eventID, ownerTeam)
	return err
}
