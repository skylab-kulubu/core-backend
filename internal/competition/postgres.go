package competition

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/competition/scoring"
	"github.com/skylab-kulubu/core-backend/internal/lifecycle"
	"github.com/skylab-kulubu/core-backend/internal/subjectlock"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

type PostgresStore struct {
	pool *pgxpool.Pool
}

func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

const competitionCols = `id, event_id, name, description, entrant_mode, status, cancel_reason,
	created_by, archived_at, archived_by, created_at, updated_at`

const stageCols = `id, competition_id, position, name, strategy, config, config_version,
	starts_at, ends_at, created_at, updated_at`

const entryCols = `id, competition_id, stage_id, entrant_id, source, value, occurred_at, recorded_at,
	recorded_by, replaces_id, revoked_at, revoked_by, revoke_reason`

func scanCompetition(row pgx.Row) (Competition, error) {
	var c Competition
	var status string
	err := row.Scan(&c.ID, &c.EventID, &c.Name, &c.Description, &c.EntrantMode, &status, &c.CancelReason,
		&c.CreatedBy, &c.ArchivedAt, &c.ArchivedBy, &c.CreatedAt, &c.UpdatedAt)
	c.Status = Status(status)
	c.Stages = []Stage{}
	return c, err
}

func scanStage(row pgx.Row) (Stage, error) {
	var st Stage
	var config []byte
	err := row.Scan(&st.ID, &st.CompetitionID, &st.Position, &st.Name, &st.Strategy, &config, &st.ConfigVersion,
		&st.StartsAt, &st.EndsAt, &st.CreatedAt, &st.UpdatedAt)
	st.Config = json.RawMessage(config)
	return st, err
}

func scanEntry(row pgx.Row) (Entry, error) {
	var e Entry
	var value *int64
	var reason *string
	err := row.Scan(&e.ID, &e.CompetitionID, &e.StageID, &e.EntrantID, &e.Source, &value, &e.OccurredAt, &e.RecordedAt,
		&e.RecordedBy, &e.ReplacesID, &e.RevokedAt, &e.RevokedBy, &reason)
	if value != nil {
		p := scoring.Points(*value)
		e.Points = &p
	}
	if reason != nil {
		e.RevokeReason = *reason
	}
	return e, err
}

// writeErr maps the database's refusals: an actor whose account is being
// deleted, and a Ticket already in an Entrant.
func writeErr(err error) error {
	if err == nil {
		return nil
	}
	if subjectlock.IsInactiveAccountReference(err) {
		return ErrForbidden
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch {
		case pg.ConstraintName == "competition_entrant_members_ticket_key":
			return ErrTicketTaken
		case pg.ConstraintName == "competition_entrant_member_same_event":
			return fmt.Errorf("%w: every ticket must be a ticket of the competition's event", ErrInvalid)
		case pg.Code == "23505":
			// Another write of the same live score got in first.
			return ErrConflict
		}
	}
	return err
}

func (s *PostgresStore) Create(ctx context.Context, c Competition, first Stage) (Competition, error) {
	config := first.Config
	if len(config) == 0 {
		config = json.RawMessage(`{}`)
	}
	var out Competition
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		created, err := scanCompetition(tx.QueryRow(ctx, `
			INSERT INTO competitions (id, event_id, name, description, entrant_mode, status, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			RETURNING `+competitionCols,
			uuid.New(), c.EventID, c.Name, c.Description, c.EntrantMode, string(c.Status), c.CreatedBy))
		if err != nil {
			return err
		}
		stage, err := scanStage(tx.QueryRow(ctx, `
			INSERT INTO competition_stages (id, competition_id, position, name, strategy, config, config_version)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			RETURNING `+stageCols,
			uuid.New(), created.ID, first.Position, first.Name, first.Strategy, []byte(config), first.ConfigVersion))
		if err != nil {
			return err
		}
		created.Stages = []Stage{stage}
		out = created
		return nil
	})
	return out, writeErr(err)
}

func (s *PostgresStore) withStages(ctx context.Context, list []Competition) ([]Competition, error) {
	if len(list) == 0 {
		return list, nil
	}
	ids := make([]uuid.UUID, len(list))
	index := make(map[uuid.UUID]int, len(list))
	for i, c := range list {
		ids[i] = c.ID
		index[c.ID] = i
	}
	rows, err := s.pool.Query(ctx, `SELECT `+stageCols+` FROM competition_stages
		WHERE competition_id = ANY($1) ORDER BY competition_id, position`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		st, err := scanStage(rows)
		if err != nil {
			return nil, err
		}
		i := index[st.CompetitionID]
		list[i].Stages = append(list[i].Stages, st)
	}
	return list, rows.Err()
}

func (s *PostgresStore) get(ctx context.Context, id uuid.UUID, includeArchived bool) (Competition, error) {
	q := `SELECT ` + competitionCols + ` FROM competitions WHERE id = $1`
	if !includeArchived {
		q += ` AND archived_at IS NULL`
	}
	c, err := scanCompetition(s.pool.QueryRow(ctx, q, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Competition{}, ErrNotFound
	}
	if err != nil {
		return Competition{}, err
	}
	list, err := s.withStages(ctx, []Competition{c})
	if err != nil {
		return Competition{}, err
	}
	return list[0], nil
}

func (s *PostgresStore) Get(ctx context.Context, id uuid.UUID) (Competition, error) {
	return s.get(ctx, id, false)
}

func (s *PostgresStore) GetIncludingArchived(ctx context.Context, id uuid.UUID) (Competition, error) {
	return s.get(ctx, id, true)
}

func (s *PostgresStore) ListByEvent(ctx context.Context, eventID uuid.UUID, visibility lifecycle.Visibility) ([]Competition, error) {
	q := `SELECT ` + competitionCols + ` FROM competitions WHERE event_id = $1`
	if condition := visibility.SQLCondition("archived_at"); condition != "" {
		q += ` AND ` + condition
	}
	rows, err := s.pool.Query(ctx, q+` ORDER BY created_at, id`, eventID)
	if err != nil {
		return nil, err
	}
	out := []Competition{}
	for rows.Next() {
		c, err := scanCompetition(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return s.withStages(ctx, out)
}

func (s *PostgresStore) Update(ctx context.Context, c Competition) (Competition, error) {
	_, err := s.pool.Exec(ctx, `
		UPDATE competitions SET name = $2, description = $3, updated_at = now()
		WHERE id = $1 AND archived_at IS NULL`, c.ID, c.Name, c.Description)
	if err != nil {
		return Competition{}, err
	}
	return s.Get(ctx, c.ID)
}

func (s *PostgresStore) Transition(ctx context.Context, id uuid.UUID, from []Status, to Status, reason string) (Competition, error) {
	statuses := make([]string, len(from))
	for i, st := range from {
		statuses[i] = string(st)
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE competitions
		SET status = $2,
			cancel_reason = CASE WHEN $2 = 'cancelled' THEN $3 ELSE cancel_reason END,
			updated_at = now()
		WHERE id = $1 AND archived_at IS NULL AND status = ANY($4)`, id, string(to), reason, statuses)
	if err != nil {
		return Competition{}, err
	}
	if tag.RowsAffected() == 0 {
		if _, err := s.Get(ctx, id); err != nil {
			return Competition{}, err
		}
		return Competition{}, ErrStatus
	}
	return s.Get(ctx, id)
}

func (s *PostgresStore) Archive(ctx context.Context, id uuid.UUID, actor *uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE competitions
		SET archived_at = COALESCE(archived_at, now()),
			archived_by = CASE WHEN archived_at IS NULL THEN $2 ELSE archived_by END,
			updated_at = CASE WHEN archived_at IS NULL THEN now() ELSE updated_at END
		WHERE id = $1`, id, actor)
	if err != nil {
		return writeErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *PostgresStore) Restore(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE competitions
		SET archived_at = NULL, archived_by = NULL,
			updated_at = CASE WHEN archived_at IS NOT NULL THEN now() ELSE updated_at END
		WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *PostgresStore) Tickets(ctx context.Context, ids []uuid.UUID) ([]TicketRef, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, event_id FROM tickets WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TicketRef{}
	for rows.Next() {
		var ref TicketRef
		if err := rows.Scan(&ref.ID, &ref.EventID); err != nil {
			return nil, err
		}
		out = append(out, ref)
	}
	return out, rows.Err()
}

func (s *PostgresStore) AddIndividuals(ctx context.Context, competitionID uuid.UUID, ticketIDs []uuid.UUID, actor *uuid.UUID) ([]uuid.UUID, error) {
	ids := make([]uuid.UUID, len(ticketIDs))
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		batch := &pgx.Batch{}
		for i, tid := range ticketIDs {
			ids[i] = uuid.New()
			// clock_timestamp keeps the Tickets' order among Entrants
			// written in one transaction.
			batch.Queue(`INSERT INTO competition_entrants (id, competition_id, kind, created_by, created_at, updated_at)
				VALUES ($1, $2, 'individual', $3, clock_timestamp(), clock_timestamp())`, ids[i], competitionID, actor)
			batch.Queue(`INSERT INTO competition_entrant_members (entrant_id, competition_id, ticket_id)
				VALUES ($1, $2, $3)`, ids[i], competitionID, tid)
		}
		return tx.SendBatch(ctx, batch).Close()
	})
	if err != nil {
		return nil, writeErr(err)
	}
	return ids, nil
}

func (s *PostgresStore) Entrants(ctx context.Context, competitionID uuid.UUID) ([]Entrant, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT e.id, e.competition_id, e.kind, e.team_name, e.status, e.status_reason, e.created_at,
			m.ticket_id, t.ticket_type, u.account_state, COALESCE(u.first_name, ''), COALESCE(u.last_name, ''),
			t.guest_first_name, t.guest_last_name
		FROM competition_entrants e
		LEFT JOIN competition_entrant_members m ON m.entrant_id = e.id
		LEFT JOIN tickets t ON t.id = m.ticket_id
		LEFT JOIN users u ON u.id = t.owner_id
		WHERE e.competition_id = $1
		ORDER BY e.created_at, e.id, m.created_at, m.ticket_id`, competitionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Entrant{}
	for rows.Next() {
		var e Entrant
		var ticketID *uuid.UUID
		var ticketType, guestFirst, guestLast *string
		var state *string
		var first, last string
		if err := rows.Scan(&e.ID, &e.CompetitionID, &e.Kind, &e.TeamName, &e.Status, &e.StatusReason, &e.CreatedAt,
			&ticketID, &ticketType, &state, &first, &last, &guestFirst, &guestLast); err != nil {
			return nil, err
		}
		if n := len(out); n == 0 || out[n-1].ID != e.ID {
			e.Members = []Member{}
			out = append(out, e)
		}
		if ticketID == nil || ticketType == nil {
			continue
		}
		var accountState user.AccountState
		if state != nil {
			accountState = user.AccountState(*state)
		}
		cur := &out[len(out)-1]
		cur.Members = append(cur.Members, Member{
			TicketID: *ticketID, TicketType: *ticketType,
			DisplayName: memberName(*ticketType, accountState, first, last, deref(guestFirst), deref(guestLast)),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		out[i].DisplayName = entrantName(out[i])
	}
	return out, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func (s *PostgresStore) SetManualScore(ctx context.Context, in ManualScore) (Entry, error) {
	var out Entry
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var live uuid.UUID
		err := tx.QueryRow(ctx, `
			SELECT id FROM competition_submissions
			WHERE stage_id = $1 AND entrant_id = $2 AND source = 'manual' AND revoked_at IS NULL
			FOR UPDATE`, in.StageID, in.EntrantID).Scan(&live)
		var replaces *uuid.UUID
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return err
		default:
			if in.Reason == "" {
				return ErrReasonRequired
			}
			if _, err := tx.Exec(ctx, `
				UPDATE competition_submissions SET revoked_at = now(), revoked_by = $2, revoke_reason = $3
				WHERE id = $1`, live, in.Actor, in.Reason); err != nil {
				return err
			}
			replaces = &live
		}
		out, err = scanEntry(tx.QueryRow(ctx, `
			INSERT INTO competition_submissions (id, competition_id, stage_id, entrant_id, source, value, recorded_by, replaces_id)
			VALUES ($1, $2, $3, $4, 'manual', $5, $6, $7)
			RETURNING `+entryCols,
			uuid.New(), in.CompetitionID, in.StageID, in.EntrantID, int64(in.Points), in.Actor, replaces))
		return err
	})
	if err != nil {
		return Entry{}, writeErr(err)
	}
	return out, nil
}

func (s *PostgresStore) Revoke(ctx context.Context, competitionID, entryID uuid.UUID, actor *uuid.UUID, reason string) (Entry, error) {
	e, err := scanEntry(s.pool.QueryRow(ctx, `
		UPDATE competition_submissions SET revoked_at = now(), revoked_by = $3, revoke_reason = $4
		WHERE id = $2 AND competition_id = $1 AND revoked_at IS NULL
		RETURNING `+entryCols, competitionID, entryID, actor, reason))
	if errors.Is(err, pgx.ErrNoRows) {
		var exists bool
		if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM competition_submissions WHERE id = $2 AND competition_id = $1)`,
			competitionID, entryID).Scan(&exists); err != nil {
			return Entry{}, err
		}
		if exists {
			return Entry{}, ErrConflict
		}
		return Entry{}, ErrNotFound
	}
	if err != nil {
		return Entry{}, writeErr(err)
	}
	return e, nil
}

func (s *PostgresStore) Entries(ctx context.Context, competitionID uuid.UUID) ([]Entry, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+entryCols+` FROM competition_submissions
		WHERE competition_id = $1 ORDER BY recorded_at, id`, competitionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Entry{}
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
