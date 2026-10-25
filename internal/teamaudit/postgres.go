package teamaudit

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// beginTimeout bounds opening a row: a database that does not answer turns
// the write into a 503 this soon, before Keycloak is asked.
const beginTimeout = 3 * time.Second

// finishTimeout bounds writing an outcome. It runs on even when the request
// is gone: Keycloak has already been asked.
const finishTimeout = 5 * time.Second

// Postgres writes rows to team_audit_records.
type Postgres struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

// NewPostgres is a writer over pool.
func NewPostgres(pool *pgxpool.Pool) *Postgres {
	return &Postgres{pool: pool, now: time.Now}
}

// SetClock replaces the writer's clock (tests).
func (p *Postgres) SetClock(now func() time.Time) { p.now = now }

func (p *Postgres) Begin(ctx context.Context, r Record) (uuid.UUID, error) {
	return p.insert(ctx, r, OutcomePending, "")
}

func (p *Postgres) Deny(ctx context.Context, r Record, errorCode string) error {
	if !ValidErrorCode(errorCode) {
		return unavailable("invalid_error_code")
	}
	_, err := p.insert(ctx, r, OutcomeDenied, errorCode)
	return err
}

func (p *Postgres) insert(ctx context.Context, r Record, outcome Outcome, errorCode string) (uuid.UUID, error) {
	if bad := r.checkOutcome(outcome); bad != "" {
		return uuid.Nil, unavailable(bad)
	}
	r, bad := r.normalize()
	if bad != "" {
		return uuid.Nil, unavailable(bad)
	}
	before, err := stateJSON(r.Before)
	if err != nil {
		return uuid.Nil, unavailable("encode")
	}
	after, err := stateJSON(r.After)
	if err != nil {
		return uuid.Nil, unavailable("encode")
	}
	ctx, cancel := context.WithTimeout(ctx, beginTimeout)
	defer cancel()
	id := uuid.New()
	_, err = p.pool.Exec(ctx, `
		INSERT INTO team_audit_records (
			id, occurred_at, actor_id, actor_basis, actor_via_group_id, action, group_id, group_path,
			target_user_id, before, after, outcome, error_code, correlation_id, client)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)`,
		id, p.now().UTC(), r.Actor.ID, string(r.Actor.Basis), nullUUID(r.Actor.ViaGroupID), string(r.Action),
		nullUUID(r.GroupID), r.GroupPath, nullID(r.TargetUserID), before, after, string(outcome), nullText(errorCode),
		r.Actor.CorrelationID, r.Actor.Client)
	if err != nil {
		return uuid.Nil, unavailable(dbErrorCode(err))
	}
	return id, nil
}

func (p *Postgres) Finish(ctx context.Context, id uuid.UUID, outcome Outcome, errorCode string, after *State) error {
	if err := checkFinish(outcome, errorCode); err != nil {
		return err
	}
	after = after.normalize()
	encoded, err := stateJSON(after)
	if err != nil {
		return err
	}
	var groupID any
	if after != nil {
		groupID = nullUUID(after.ID)
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
	defer cancel()
	tag, err := p.pool.Exec(ctx, `
		UPDATE team_audit_records
		SET outcome = $2, error_code = $3, after = COALESCE($4, after), group_id = COALESCE(group_id, $5)
		WHERE id = $1 AND outcome = 'pending'`,
		id, string(outcome), nullText(errorCode), encoded, groupID)
	if err != nil {
		return errors.New("teamaudit: finish: " + dbErrorCode(err))
	}
	if tag.RowsAffected() != 1 {
		return ErrNotPending
	}
	return nil
}

// readTimeout bounds a read of the app rows.
const readTimeout = 5 * time.Second

// appColumns are a row's columns as scanRow reads them.
const appColumns = `id::text, occurred_at, actor_id::text, actor_basis, COALESCE(actor_via_group_id::text, ''), action,
	COALESCE(group_id::text, ''), group_path, COALESCE(target_user_id::text, ''), before, after, outcome,
	COALESCE(error_code, ''), correlation_id, client`

func (p *Postgres) AppRows(ctx context.Context, client string, limit int) ([]Row, error) {
	return p.appRows(ctx, `
		SELECT `+appColumns+` FROM team_audit_records
		WHERE action IN ('app.assign', 'app.unassign') AND after->>'client' = $1
		ORDER BY occurred_at DESC, id DESC
		LIMIT $2`, client, limit)
}

func (p *Postgres) LatestAppRows(ctx context.Context, client string) ([]Row, error) {
	return p.appRows(ctx, `
		SELECT DISTINCT ON (COALESCE(group_id::text, target_user_id::text), after->>'role') `+appColumns+`
		FROM team_audit_records
		WHERE action IN ('app.assign', 'app.unassign') AND outcome = 'ok' AND after->>'client' = $1
		ORDER BY COALESCE(group_id::text, target_user_id::text), after->>'role', occurred_at DESC, id DESC`, client)
}

func (p *Postgres) appRows(ctx context.Context, query string, args ...any) ([]Row, error) {
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	rows, err := p.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, errors.New("teamaudit: read: " + dbErrorCode(err))
	}
	defer rows.Close()
	out := []Row{}
	for rows.Next() {
		var (
			r                               Row
			id, actor, group, target, basis string
			action, outcome                 string
			before, after                   []byte
		)
		if err := rows.Scan(&id, &r.OccurredAt, &actor, &basis, &r.Actor.ViaGroupID, &action, &group, &r.GroupPath,
			&target, &before, &after, &outcome, &r.ErrorCode, &r.Actor.CorrelationID, &r.Actor.Client); err != nil {
			return nil, errors.New("teamaudit: read: " + dbErrorCode(err))
		}
		r.ID, _ = uuid.Parse(id)
		r.Actor.ID, _ = uuid.Parse(actor)
		r.Actor.Basis, r.Action, r.Outcome, r.GroupID = Basis(basis), Action(action), Outcome(outcome), group
		if target != "" {
			r.TargetUserID, _ = uuid.Parse(target)
		}
		if r.Before, err = stateFrom(before); err != nil {
			return nil, err
		}
		if r.After, err = stateFrom(after); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, errors.New("teamaudit: read: " + dbErrorCode(err))
	}
	return out, nil
}

// stateFrom is the jsonb column raw as a State, nil for none.
func stateFrom(raw []byte) (*State, error) {
	if raw == nil {
		return nil, nil
	}
	var s State
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, errors.New("teamaudit: read: state")
	}
	return &s, nil
}

// stateJSON is s as the jsonb column holds it, nil for none.
func stateJSON(s *State) ([]byte, error) {
	if s == nil {
		return nil, nil
	}
	return json.Marshal(s)
}

// nullUUID is s as a uuid, NULL when it is empty or not one.
func nullUUID(s string) any {
	id, err := uuid.Parse(s)
	if err != nil {
		return nil
	}
	return id
}

func nullID(id uuid.UUID) any {
	if id == uuid.Nil {
		return nil
	}
	return id
}

func nullText(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// dbErrorCode is a short code for a database error: its SQLSTATE, or the
// kind of failure. Never the message, which can quote a value.
func dbErrorCode(err error) string {
	var pg *pgconn.PgError
	switch {
	case errors.As(err, &pg):
		return "sqlstate_" + strings.ToLower(pg.Code)
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	}
	return "database"
}
