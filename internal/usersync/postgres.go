package usersync

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// LockName names the session advisory lock one pass holds, so the replicas
// of a release pass one at a time.
const LockName = "core-backend user sync"

// Postgres is Rows on core's database.
type Postgres struct {
	pool *pgxpool.Pool
}

// NewPostgres builds the rows on pool.
func NewPostgres(pool *pgxpool.Pool) *Postgres {
	return &Postgres{pool: pool}
}

// Lock holds a connection of its own for the pass, since the lock is the
// session's. Release unlocks and gives the connection back; when the unlock
// fails it closes the connection instead, which ends the session and so the
// lock.
func (p *Postgres) Lock(ctx context.Context) (func(), bool, error) {
	conn, err := p.pool.Acquire(ctx)
	if err != nil {
		return nil, false, err
	}
	closeConn := func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = conn.Hijack().Close(closeCtx)
	}
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1, 0))`, LockName).Scan(&locked); err != nil {
		closeConn()
		return nil, false, err
	}
	if !locked {
		conn.Release()
		return nil, false, nil
	}
	release := func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var unlocked bool
		if err := conn.QueryRow(unlockCtx, `SELECT pg_advisory_unlock(hashtextextended($1, 0))`, LockName).Scan(&unlocked); err != nil || !unlocked {
			closeConn()
			return
		}
		conn.Release()
	}
	return release, true, nil
}

func (p *Postgres) States(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]State, error) {
	out := make(map[uuid.UUID]State, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := p.pool.Query(ctx, `
		SELECT id, email, first_name, last_name, sky_number, account_state = 'active', welcome_sent_at IS NOT NULL
		  FROM users
		 WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var st State
		if err := rows.Scan(&id, &st.Email, &st.FirstName, &st.LastName, &st.SkyNumber, &st.Active, &st.Welcomed); err != nil {
			return nil, err
		}
		out[id] = st
	}
	return out, rows.Err()
}

func (p *Postgres) FillMissing(ctx context.Context, id uuid.UUID, email, firstName, lastName string) (bool, error) {
	tag, err := p.pool.Exec(ctx, `
		UPDATE users SET
			email = CASE WHEN email = '' THEN $2 ELSE email END,
			first_name = CASE WHEN first_name = '' THEN $3 ELSE first_name END,
			last_name = CASE WHEN last_name = '' THEN $4 ELSE last_name END,
			updated_at = now()
		 WHERE id = $1
		   AND account_state = 'active'
		   AND ((email = '' AND $2 <> '') OR (first_name = '' AND $3 <> '') OR (last_name = '' AND $4 <> ''))`,
		id, email, firstName, lastName)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (p *Postgres) ClaimWelcome(ctx context.Context, id uuid.UUID) (user.User, time.Time, bool, error) {
	var u user.User
	var at time.Time
	err := p.pool.QueryRow(ctx, `
		UPDATE users SET welcome_sent_at = clock_timestamp()
		 WHERE id = $1
		   AND welcome_sent_at IS NULL
		   AND email <> ''
		   AND account_state = 'active'
		RETURNING id, email, first_name, last_name, username, school_email, sky_number, created_at, welcome_sent_at`, id).Scan(
		&u.ID, &u.Email, &u.FirstName, &u.LastName, &u.Username, &u.SchoolEmail, &u.SkyNumber, &u.CreatedAt, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		return user.User{}, time.Time{}, false, nil
	}
	if err != nil {
		return user.User{}, time.Time{}, false, err
	}
	return u, at, true, nil
}

func (p *Postgres) ReleaseWelcome(ctx context.Context, id uuid.UUID, at time.Time) error {
	_, err := p.pool.Exec(ctx, `UPDATE users SET welcome_sent_at = NULL WHERE id = $1 AND welcome_sent_at = $2`, id, at)
	return err
}
