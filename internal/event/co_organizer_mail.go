package event

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// CoOrganizerMailDelay is how long a team's Leaders or a person added to, or
// removed from, an Event's Co-organizers wait for their mail
// (docs/co-organizers.md#mails): a change taken back within it tells nobody.
const CoOrganizerMailDelay = 2 * time.Minute

// Kinds of a queued Co-organizer mail (co_organizer_notifications.kind).
const (
	CoOrganizerMailAdded   = "added"
	CoOrganizerMailRemoved = "removed"
)

// NotifyCoOrganizers turns the Co-organizer mail on or off: while on, a
// write of an Event's Co-organizer list also queues the mail of each team
// and person added or removed in co_organizer_notifications, in the same
// transaction, for the worker (internal/coorganizermail). Off, nothing is
// queued, so turning it on later tells none of the changes made meanwhile.
// A team whose choices alone changed is never told.
func (s *PostgresStore) NotifyCoOrganizers(on bool) { s.notifyCoOrganizers.Store(on) }

// queueCoOrganizerMail queues a row for each team and person w adds to, or
// removes from, the lists the Event had (was); a row still waiting for the
// same Event, kind and team or person stays as it is.
func (s *PostgresStore) queueCoOrganizerMail(ctx context.Context, tx pgx.Tx, w CoOrganizersWrite, wasTeams []CoOrganizerTeam, wasPeople []uuid.UUID) error {
	if !s.notifyCoOrganizers.Load() {
		return nil
	}
	hasTeam := func(list []CoOrganizerTeam, code string) bool {
		return slices.ContainsFunc(list, func(t CoOrganizerTeam) bool { return strings.EqualFold(t.Code, code) })
	}
	queueTeam := func(kind, code string) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO co_organizer_notifications (id, event_id, kind, team_code, actor_id, next_attempt_at)
			VALUES ($1, $2, $3, $4, $5, now() + make_interval(secs => $6))
			ON CONFLICT (event_id, kind, team_code) WHERE team_code IS NOT NULL DO NOTHING`,
			uuid.New(), w.EventID, kind, code, w.By, CoOrganizerMailDelay.Seconds())
		return err
	}
	queuePerson := func(kind string, person uuid.UUID) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO co_organizer_notifications (id, event_id, kind, user_id, actor_id, next_attempt_at)
			SELECT $1, $2, $3, u.id, $5, now() + make_interval(secs => $6) FROM users u WHERE u.id = $4
			ON CONFLICT (event_id, kind, user_id) WHERE user_id IS NOT NULL DO NOTHING`,
			uuid.New(), w.EventID, kind, person, w.By, CoOrganizerMailDelay.Seconds())
		return err
	}
	for _, team := range w.Teams {
		if !hasTeam(wasTeams, team.Code) {
			if err := queueTeam(CoOrganizerMailAdded, team.Code); err != nil {
				return err
			}
		}
	}
	for _, team := range wasTeams {
		if !hasTeam(w.Teams, team.Code) {
			if err := queueTeam(CoOrganizerMailRemoved, team.Code); err != nil {
				return err
			}
		}
	}
	for _, person := range w.People {
		if !slices.Contains(wasPeople, person) {
			if err := queuePerson(CoOrganizerMailAdded, person); err != nil {
				return err
			}
		}
	}
	for _, person := range wasPeople {
		if !slices.Contains(w.People, person) {
			if err := queuePerson(CoOrganizerMailRemoved, person); err != nil {
				return err
			}
		}
	}
	return nil
}
