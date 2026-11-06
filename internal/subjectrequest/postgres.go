package subjectrequest

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresStore keeps the register in subject_requests and its trail in
// subject_request_events (migration 20261027120000).
type PostgresStore struct {
	pool *pgxpool.Pool
}

func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

const requestCols = `id, to_char(received_on, 'YYYY-MM-DD'), channel, requester_name, requester_contact, user_id,
	request_types, status, to_char(due_on, 'YYYY-MM-DD'), to_char(answered_on, 'YYYY-MM-DD'), answer_summary, responder,
	created_at, created_by, updated_at, updated_by`

func scanRequest(row pgx.Row) (Request, error) {
	var r Request
	var types []string
	err := row.Scan(&r.ID, &r.ReceivedOn, &r.Channel, &r.RequesterName, &r.RequesterContact, &r.UserID,
		&types, &r.Status, &r.DueOn, &r.AnsweredOn, &r.AnswerSummary, &r.Responder,
		&r.CreatedAt, &r.CreatedBy, &r.UpdatedAt, &r.UpdatedBy)
	if err != nil {
		return Request{}, err
	}
	r.RequestTypes = make([]Type, len(types))
	for i, t := range types {
		r.RequestTypes[i] = Type(t)
	}
	return r, nil
}

func (s *PostgresStore) List(ctx context.Context, limit int) ([]Request, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+requestCols+` FROM subject_requests
		ORDER BY answered_on IS NOT NULL, CASE WHEN answered_on IS NULL THEN due_on END, answered_on DESC, received_on DESC, id
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Request, 0)
	for rows.Next() {
		r, err := scanRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *PostgresStore) Get(ctx context.Context, id uuid.UUID) (Request, error) {
	r, err := scanRequest(s.pool.QueryRow(ctx, `SELECT `+requestCols+` FROM subject_requests WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Request{}, ErrNotFound
	}
	return r, err
}

func (s *PostgresStore) Events(ctx context.Context, id uuid.UUID) ([]Event, error) {
	rows, err := s.pool.Query(ctx, `SELECT occurred_at, actor_id, action, fields, status FROM subject_request_events
		WHERE request_id = $1 ORDER BY occurred_at, id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Event, 0)
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.At, &e.ActorID, &e.Action, &e.Fields, &e.Status); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func typeStrings(types []Type) []string {
	out := make([]string, len(types))
	for i, t := range types {
		out[i] = string(t)
	}
	return out
}

func (s *PostgresStore) Create(ctx context.Context, r Request, e Event) (Request, error) {
	return s.write(ctx, r, e, `INSERT INTO subject_requests (id, received_on, channel, requester_name, requester_contact, user_id,
			request_types, status, answered_on, answer_summary, responder, created_at, created_by, updated_at, updated_by)
		VALUES ($1, $2::date, $3, $4, $5, $6, $7, $8, $9::date, $10, $11, $12, $13, $12, $13)
		RETURNING `+requestCols, r.CreatedAt, r.CreatedBy)
}

func (s *PostgresStore) Update(ctx context.Context, r Request, e Event) (Request, error) {
	return s.write(ctx, r, e, `UPDATE subject_requests SET received_on = $2::date, channel = $3, requester_name = $4,
			requester_contact = $5, user_id = $6, request_types = $7, status = $8, answered_on = $9::date,
			answer_summary = $10, responder = $11, updated_at = $12, updated_by = $13
		WHERE id = $1
		RETURNING `+requestCols, r.UpdatedAt, r.UpdatedBy)
}

// write runs the request's statement and its trail row in one transaction.
// The statement takes the request's fields as $1–$11 and at, by as $12, $13.
func (s *PostgresStore) write(ctx context.Context, r Request, e Event, statement string, at, by any) (Request, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Request{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	out, err := scanRequest(tx.QueryRow(ctx, statement, r.ID, r.ReceivedOn, r.Channel, r.RequesterName, r.RequesterContact,
		r.UserID, typeStrings(r.RequestTypes), r.Status, r.AnsweredOn, r.AnswerSummary, r.Responder, at, by))
	if errors.Is(err, pgx.ErrNoRows) {
		return Request{}, ErrNotFound
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23503" {
		// The only reference is the account: user_id names no User.
		return Request{}, invalid("user_unknown")
	}
	if err != nil {
		return Request{}, fmt.Errorf("subject request: %w", err)
	}
	fields := e.Fields
	if fields == nil {
		fields = []string{}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO subject_request_events (request_id, occurred_at, actor_id, action, fields, status)
		VALUES ($1, $2, $3, $4, $5, $6)`, r.ID, e.At, e.ActorID, e.Action, fields, e.Status); err != nil {
		return Request{}, fmt.Errorf("subject request trail: %w", err)
	}
	return out, tx.Commit(ctx)
}
