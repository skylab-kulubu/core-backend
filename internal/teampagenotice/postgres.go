package teampagenotice

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresLedger keeps the ledger in team_page_notices (migration
// 20261030100000): one row per person sent the notice.
type PostgresLedger struct {
	pool *pgxpool.Pool
}

func NewPostgresLedger(pool *pgxpool.Pool) *PostgresLedger {
	return &PostgresLedger{pool: pool}
}

func (l *PostgresLedger) Notified(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]bool, error) {
	out := map[uuid.UUID]bool{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := l.pool.Query(ctx, `SELECT user_id FROM team_page_notices WHERE user_id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

func (l *PostgresLedger) Claim(ctx context.Context, id uuid.UUID, at time.Time) (bool, error) {
	tag, err := l.pool.Exec(ctx, `
		INSERT INTO team_page_notices (user_id, sent_at) VALUES ($1, $2)
		ON CONFLICT (user_id) DO NOTHING`, id, at)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (l *PostgresLedger) Release(ctx context.Context, id uuid.UUID) error {
	_, err := l.pool.Exec(ctx, `DELETE FROM team_page_notices WHERE user_id = $1`, id)
	return err
}
