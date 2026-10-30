package teamcode

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// LockName is the Postgres advisory lock every Group create and rename
// holds, on every core, from its name check to its Keycloak write.
const LockName = "core.group_names"

// ErrUnavailable is a lock or a check the database could not give: the
// Group write was not made, Keycloak was not asked. Callers answer it as
// the Team audit record's 503 (audit_unavailable): no record, no change.
var ErrUnavailable = errors.New("teamcode: database unavailable")

// PostgresLedger keeps the retired codes in retired_team_codes, reads the
// Owner team records from their tables, and locks with a transaction-scoped
// advisory lock, which a core that dies mid-write gives back with its
// connection.
type PostgresLedger struct {
	pool *pgxpool.Pool
	// turn lets one Locked of this process at a time take a connection and
	// wait for the advisory lock: the others wait here, holding none, so
	// writes queued behind the lock never use up the pool that the lock
	// holder and every other route need (review of #235, B2). The advisory
	// lock still orders the processes.
	turn chan struct{}
}

func NewPostgresLedger(pool *pgxpool.Pool) *PostgresLedger {
	return &PostgresLedger{pool: pool, turn: make(chan struct{}, 1)}
}

// lockTimeout bounds the wait for the advisory lock held by another core.
const lockTimeout = "10s"

// Locked runs write in a transaction that holds LockName. A code write
// retires is committed with the transaction, after write's Keycloak call has
// succeeded; when that call fails, nothing is retired. When the commit
// itself fails after Keycloak took the write, the error says which code was
// left unretired (a code, never a person).
func (l *PostgresLedger) Locked(ctx context.Context, write func(ctx context.Context, records Records) error) error {
	select {
	case l.turn <- struct{}{}:
		defer func() { <-l.turn }()
	case <-ctx.Done():
		return fmt.Errorf("%w: waiting for the lock: %w", ErrUnavailable, ctx.Err())
	}
	tx, err := l.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("%w: begin: %w", ErrUnavailable, err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '`+lockTimeout+`'`); err != nil {
		return fmt.Errorf("%w: lock timeout: %w", ErrUnavailable, err)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, LockName); err != nil {
		return fmt.Errorf("%w: lock: %w", ErrUnavailable, err)
	}
	records := &postgresRecords{tx: tx}
	if err := write(ctx, records); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		if len(records.retired) > 0 {
			return fmt.Errorf("teamcode: the Group was written but retiring %v was not: %w", records.retired, err)
		}
		return fmt.Errorf("teamcode: commit: %w", err)
	}
	return nil
}

type postgresRecords struct {
	tx      pgx.Tx
	retired []string
}

// Held's last lookup reads retired_team_codes through its unique
// lower(code) index.
func (r *postgresRecords) Held(ctx context.Context, code string) (bool, error) {
	var held bool
	err := r.tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM events WHERE lower(owner_team) = lower($1))
			OR EXISTS (SELECT 1 FROM certificates WHERE lower(owner_team) = lower($1))
			OR EXISTS (SELECT 1 FROM certificate_templates WHERE lower(owner_team) = lower($1))
			OR EXISTS (SELECT 1 FROM certificate_template_bindings WHERE scope = 'ownerTeam' AND lower(scope_key) = lower($1))
			OR EXISTS (SELECT 1 FROM retired_team_codes WHERE lower(code) = lower($1))`, code).Scan(&held)
	if err != nil {
		return false, fmt.Errorf("%w: held: %w", ErrUnavailable, err)
	}
	return held, nil
}

func (r *postgresRecords) References(ctx context.Context, code string) ([]string, error) {
	var event, certificate, template, binding bool
	err := r.tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM events WHERE lower(owner_team) = lower($1)),
			EXISTS (SELECT 1 FROM certificates WHERE lower(owner_team) = lower($1)),
			EXISTS (SELECT 1 FROM certificate_templates WHERE lower(owner_team) = lower($1)),
			EXISTS (SELECT 1 FROM certificate_template_bindings WHERE scope = 'ownerTeam' AND lower(scope_key) = lower($1))`, code).
		Scan(&event, &certificate, &template, &binding)
	if err != nil {
		return nil, fmt.Errorf("%w: references: %w", ErrUnavailable, err)
	}
	var out []string
	for i, held := range []bool{event, certificate, template, binding} {
		if held {
			out = append(out, recordKinds[i])
		}
	}
	return out, nil
}

// Retire keeps the first row of a code: retiring it again, in any case, is
// stopped by the unique lower(code) index and changes nothing.
func (r *postgresRecords) Retire(ctx context.Context, code, groupID, reason string) error {
	var group *uuid.UUID
	if id, err := uuid.Parse(groupID); err == nil {
		group = &id
	}
	if code == "" {
		return errors.New("teamcode: retire: no code")
	}
	if _, err := r.tx.Exec(ctx, `
		INSERT INTO retired_team_codes (code, group_id, reason) VALUES ($1, $2, $3)
		ON CONFLICT (lower(code)) DO NOTHING`, code, group, reason); err != nil {
		return fmt.Errorf("teamcode: retire: %w", err)
	}
	r.retired = append(r.retired, code)
	return nil
}

// RecordCodes are the codes core's Owner team records name (Records.Held's
// tables, archived and deleted rows included), blanks around each trimmed,
// each with the kinds of record that name it (RecordEvent, …, in that
// order). Blank codes are left out. Codes and kinds, never a record's id.
func (l *PostgresLedger) RecordCodes(ctx context.Context) (map[string][]string, error) {
	rows, err := l.pool.Query(ctx, `
		SELECT DISTINCT code, kind FROM (
			SELECT btrim(owner_team) AS code, 0 AS kind FROM events
			UNION ALL SELECT btrim(owner_team), 1 FROM certificates
			UNION ALL SELECT btrim(owner_team), 2 FROM certificate_templates
			UNION ALL SELECT btrim(scope_key), 3 FROM certificate_template_bindings WHERE scope = 'ownerTeam'
		) AS named
		WHERE code IS NOT NULL AND code <> ''
		ORDER BY code, kind`)
	if err != nil {
		return nil, fmt.Errorf("%w: record codes: %w", ErrUnavailable, err)
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var (
			code string
			kind int
		)
		if err := rows.Scan(&code, &kind); err != nil {
			return nil, fmt.Errorf("%w: record codes: %w", ErrUnavailable, err)
		}
		out[code] = append(out[code], recordKinds[kind])
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: record codes: %w", ErrUnavailable, err)
	}
	return out, nil
}

// RetiredCodes are the codes retired_team_codes holds, as written.
func (l *PostgresLedger) RetiredCodes(ctx context.Context) ([]string, error) {
	rows, err := l.pool.Query(ctx, `SELECT code FROM retired_team_codes ORDER BY code`)
	if err != nil {
		return nil, fmt.Errorf("%w: retired codes: %w", ErrUnavailable, err)
	}
	codes, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("%w: retired codes: %w", ErrUnavailable, err)
	}
	return codes, nil
}

// SeedRetired retires, with ReasonSeeded and no Group id, every code that
// codes answers and no retired code holds yet (compared without case), in
// one transaction holding LockName. codes runs inside the lock, so it sees
// every Group a create or a rename made before it, and none is made while it
// runs: it answers the codes that still have no Group. It answers the codes
// written, in codes' order; a second run writes nothing new. On an error
// nothing is written.
func (l *PostgresLedger) SeedRetired(ctx context.Context, codes func(ctx context.Context) ([]string, error)) ([]string, error) {
	var written []string
	err := l.Locked(ctx, func(ctx context.Context, records Records) error {
		r, ok := records.(*postgresRecords)
		if !ok {
			return errors.New("teamcode: seed: not a Postgres transaction")
		}
		want, err := codes(ctx)
		if err != nil {
			return err
		}
		for _, code := range want {
			if code == "" {
				continue
			}
			var retired bool
			if err := r.tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM retired_team_codes WHERE lower(code) = lower($1))`, code).Scan(&retired); err != nil {
				return fmt.Errorf("%w: seed: %w", ErrUnavailable, err)
			}
			if retired {
				continue
			}
			if err := r.Retire(ctx, code, "", ReasonSeeded); err != nil {
				return err
			}
			written = append(written, code)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return written, nil
}
