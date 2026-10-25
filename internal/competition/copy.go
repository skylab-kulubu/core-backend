package competition

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// CopyForEvent copies the source Event's current Competitions and their
// stages into the copy Event, inside the caller's transaction (event copy,
// ticket 06; the copy of one Competition into another Event, competition
// ticket 29, is to use it too). A copied Competition starts as a draft with
// its name, description and entrant mode; each stage keeps its position,
// name, strategy, config and config version, its dates moved by shift.
// Entrants and their members, submissions, adjustments, results, the
// cancel reason and archived Competitions are not copied. by is who
// copies: the copies' creator when they have an account row.
func CopyForEvent(ctx context.Context, tx pgx.Tx, source, copy uuid.UUID, shift func(time.Time) time.Time, by *uuid.UUID) error {
	rows, err := tx.Query(ctx, `
		SELECT id, name, description, entrant_mode FROM competitions
		WHERE event_id = $1 AND archived_at IS NULL
		ORDER BY created_at, id`, source)
	if err != nil {
		return err
	}
	competitions, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Competition, error) {
		var c Competition
		err := row.Scan(&c.ID, &c.Name, &c.Description, &c.EntrantMode)
		return c, err
	})
	if err != nil {
		return err
	}
	for _, c := range competitions {
		copiedID := uuid.New()
		if _, err := tx.Exec(ctx, `
			INSERT INTO competitions (id, event_id, name, description, entrant_mode, status, created_by)
			VALUES ($1, $2, $3, $4, $5, 'draft', (SELECT id FROM users WHERE id = $6))`,
			copiedID, copy, c.Name, c.Description, c.EntrantMode, by); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT position, name, strategy, config, config_version, starts_at, ends_at
			FROM competition_stages WHERE competition_id = $1 ORDER BY position`, c.ID)
		if err != nil {
			return err
		}
		stages, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Stage, error) {
			var st Stage
			err := row.Scan(&st.Position, &st.Name, &st.Strategy, &st.Config, &st.ConfigVersion, &st.StartsAt, &st.EndsAt)
			return st, err
		})
		if err != nil {
			return err
		}
		for _, st := range stages {
			if _, err := tx.Exec(ctx, `
				INSERT INTO competition_stages (id, competition_id, position, name, strategy, config, config_version, starts_at, ends_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
				uuid.New(), copiedID, st.Position, st.Name, st.Strategy, []byte(st.Config), st.ConfigVersion,
				moved(st.StartsAt, shift), moved(st.EndsAt, shift)); err != nil {
				return err
			}
		}
	}
	return nil
}

func moved(t *time.Time, shift func(time.Time) time.Time) *time.Time {
	if t == nil {
		return nil
	}
	m := shift(*t)
	return &m
}
