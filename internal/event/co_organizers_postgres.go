package event

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/subjectlock"
)

// SetCoOrganizers writes the difference between the Event's Co-organizer
// rows and w's lists in one transaction, under the Event's row lock
// (docs/co-organizers.md): the rows that go are deleted, the new ones
// inserted, a team that stays has its choices updated; a row that stays
// keeps its added_by and added_at, and only a new person must be an active
// account.
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
	wasTeams, wasPeople, err := readCoOrganizers(ctx, tx, w.EventID)
	if err != nil {
		return Event{}, err
	}
	if w.Expect && !sameCoOrganizers(wasTeams, wasPeople, w.WasTeams, w.WasPeople) {
		return Event{}, ErrConflict
	}
	codes := make([]string, len(w.Teams))
	for i, team := range w.Teams {
		codes[i] = team.Code
	}
	people := w.People
	if people == nil {
		people = []uuid.UUID{}
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM event_co_organizers
		WHERE event_id = $1
		  AND (team_code IS NOT NULL AND NOT team_code = ANY($2::text[])
		    OR user_id IS NOT NULL AND NOT user_id = ANY($3::uuid[]))`, w.EventID, codes, people); err != nil {
		return Event{}, err
	}
	for _, team := range w.Teams {
		if _, err := tx.Exec(ctx, `
			INSERT INTO event_co_organizers (event_id, team_code, management, visibility, added_by)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (event_id, team_code) DO UPDATE SET management = EXCLUDED.management, visibility = EXCLUDED.visibility
			WHERE (event_co_organizers.management, event_co_organizers.visibility) IS DISTINCT FROM (EXCLUDED.management, EXCLUDED.visibility)`,
			w.EventID, team.Code, string(team.Management), string(team.Visibility), w.By); err != nil {
			return Event{}, err
		}
	}
	for _, person := range w.People {
		// A person already on the list is no insert at all: the account
		// guard (a BEFORE INSERT trigger, which runs before any ON
		// CONFLICT) looks at the new ones alone.
		if _, err := tx.Exec(ctx, `
			INSERT INTO event_co_organizers (event_id, user_id, added_by)
			SELECT $1::uuid, $2::uuid, $3::uuid
			WHERE NOT EXISTS (SELECT 1 FROM event_co_organizers WHERE event_id = $1 AND user_id = $2)`, w.EventID, person, w.By); err != nil {
			if subjectlock.IsInactiveAccountReference(err) {
				return Event{}, &InactivePersonError{ID: person}
			}
			return Event{}, err
		}
	}
	if err := s.queueCoOrganizerMail(ctx, tx, w, wasTeams, wasPeople); err != nil {
		return Event{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Event{}, err
	}
	return s.Get(ctx, w.EventID)
}

// readCoOrganizers reads the Event's Co-organizer rows inside tx.
func readCoOrganizers(ctx context.Context, tx pgx.Tx, eventID uuid.UUID) ([]CoOrganizerTeam, []uuid.UUID, error) {
	rows, err := tx.Query(ctx, `SELECT team_code, user_id, management, visibility FROM event_co_organizers WHERE event_id = $1`, eventID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var e Event
	for rows.Next() {
		var code, management, visibility *string
		var userID *uuid.UUID
		if err := rows.Scan(&code, &userID, &management, &visibility); err != nil {
			return nil, nil, err
		}
		e.addCoOrganizer(code, userID, management, visibility)
	}
	return e.CoOrganizers, e.CoOrganizerIDs, rows.Err()
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

// coOrganizersSQL reads the Co-organizer rows of the Events $1 names, in
// the columns of withGalleriesAndDoorStaff's people query: the event, true,
// the team's code, the person, the team's two choices.
const coOrganizersSQL = `
		SELECT event_id, true, team_code, user_id, management, visibility
		FROM event_co_organizers WHERE event_id = ANY($1)`

// addCoOrganizer adds a Co-organizer row as the store read it: a team with
// both choices, or a person.
func (e *Event) addCoOrganizer(code *string, userID *uuid.UUID, management, visibility *string) {
	switch {
	case code != nil && management != nil && visibility != nil:
		e.CoOrganizers = append(e.CoOrganizers, CoOrganizerTeam{
			Code: *code, Management: Management(*management), Visibility: Visibility(*visibility),
		})
	case userID != nil:
		e.CoOrganizerIDs = append(e.CoOrganizerIDs, *userID)
	}
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

// HasCoOrganizerSeat reports whether p holds a seat on a current Event as
// one of its Co-organizers (media.CoOrganizerSeats, docs/co-organizers.md):
// a person row, or a team row of a team p leads (its leader subgroup), or
// of a "members" team p is in; a row only while the Event's rights last
// (CoOrganizerPersonGrace after its end, or after its start plus a day; an
// Event with no date never ends). A read that fails is no seat.
func (s *PostgresStore) HasCoOrganizerSeat(ctx context.Context, p authz.Principal) bool {
	var person *uuid.UUID
	if id, err := uuid.Parse(p.ID); err == nil {
		person = &id
	}
	members, leads := seatTeams(p)
	if person == nil && len(members) == 0 {
		return false
	}
	var seat bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM event_co_organizers c
			JOIN events e ON e.id = c.event_id AND e.archived_at IS NULL
			WHERE (c.user_id = $1
			    OR c.team_code = ANY($2::text[])
			    OR (c.management = 'members' AND c.team_code = ANY($3::text[])))
			  AND (COALESCE(e.end_date, e.start_date + interval '1 day') IS NULL
			    OR COALESCE(e.end_date, e.start_date + interval '1 day') + make_interval(secs => $4) > now()))`,
		person, leads, members, CoOrganizerPersonGrace.Seconds()).Scan(&seat)
	return err == nil && seat
}

// seatTeams are the team codes p's Groups name, each segment of each path
// (members), and those p leads: the segment right above a leader subgroup
// (leads). Never nil.
func seatTeams(p authz.Principal) (members, leads []string) {
	members, leads = []string{}, []string{}
	leaders := authz.DefaultPolicy().LeaderSubgroups
	for _, group := range p.Groups {
		segments := strings.Split(strings.Trim(group, "/"), "/")
		for i, segment := range segments {
			if segment == "" {
				continue
			}
			if !slices.Contains(members, segment) {
				members = append(members, segment)
			}
			if i == len(segments)-2 && slices.Contains(leaders, segments[i+1]) && !slices.Contains(leads, segment) {
				leads = append(leads, segment)
			}
		}
	}
	return members, leads
}
