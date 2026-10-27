package membershipterm

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresStore keeps the terms in management_membership_terms, their
// mails in management_term_mails and the runs in management_term_runs.
type PostgresStore struct {
	pool *pgxpool.Pool
}

func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

var _ Store = (*PostgresStore)(nil)

const termColumns = `id, user_id, group_id, group_path, scope, ends_at, season_id, granted_by, source, started_at,
	warned_14d_at, warned_1d_at, expired_at, ended_at, updated_at`

func scanTerm(row pgx.Row) (Term, error) {
	var t Term
	var scope, source string
	err := row.Scan(&t.ID, &t.UserID, &t.GroupID, &t.GroupPath, &scope, &t.EndsAt, &t.SeasonID, &t.GrantedBy, &source,
		&t.StartedAt, &t.Warned14d, &t.Warned1d, &t.ExpiredAt, &t.EndedAt, &t.UpdatedAt)
	t.Scope, t.Source = Scope(scope), Source(source)
	return t, err
}

func (s *PostgresStore) terms(ctx context.Context, query string, args ...any) ([]Term, error) {
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Term{}
	for rows.Next() {
		t, err := scanTerm(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *PostgresStore) Open(ctx context.Context) ([]Term, error) {
	return s.terms(ctx, `SELECT `+termColumns+` FROM management_membership_terms
		WHERE ended_at IS NULL ORDER BY ends_at NULLS LAST, group_path, user_id`)
}

func (s *PostgresStore) OpenFor(ctx context.Context, userID uuid.UUID) ([]Term, error) {
	return s.terms(ctx, `SELECT `+termColumns+` FROM management_membership_terms
		WHERE ended_at IS NULL AND user_id = $1 ORDER BY ends_at NULLS LAST, group_path`, userID)
}

func (s *PostgresStore) Get(ctx context.Context, id uuid.UUID) (Term, error) {
	t, err := scanTerm(s.pool.QueryRow(ctx, `SELECT `+termColumns+` FROM management_membership_terms WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Term{}, ErrNotFound
	}
	return t, err
}

const insertTerm = `
	INSERT INTO management_membership_terms
		(id, user_id, group_id, group_path, scope, ends_at, season_id, granted_by, source, started_at, updated_at)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $10)
	ON CONFLICT (user_id, group_id) WHERE ended_at IS NULL DO NOTHING`

func insertArgs(t Term) []any {
	if t.ID == uuid.Nil {
		t.ID = uuid.New()
	}
	return []any{t.ID, t.UserID, t.GroupID, t.GroupPath, string(t.Scope), t.EndsAt, t.SeasonID, t.GrantedBy, string(t.Source), t.StartedAt}
}

func (s *PostgresStore) Insert(ctx context.Context, terms []Term) (int, error) {
	if len(terms) == 0 {
		return 0, nil
	}
	batch := &pgx.Batch{}
	for _, t := range terms {
		batch.Queue(insertTerm, insertArgs(t)...)
	}
	results := s.pool.SendBatch(ctx, batch)
	defer results.Close()
	added := 0
	for range terms {
		tag, err := results.Exec()
		if err != nil {
			return added, err
		}
		added += int(tag.RowsAffected())
	}
	return added, results.Close()
}

func (s *PostgresStore) Restart(ctx context.Context, t Term) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			UPDATE management_membership_terms SET ended_at = $3, updated_at = $3
			WHERE user_id = $1 AND group_id = $2 AND ended_at IS NULL`, t.UserID, t.GroupID, t.StartedAt); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, insertTerm, insertArgs(t)...)
		return err
	})
}

func (s *PostgresStore) End(ctx context.Context, userID, groupID uuid.UUID, at time.Time) (int, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE management_membership_terms SET ended_at = $3, updated_at = $3
		WHERE user_id = $1 AND group_id = $2 AND ended_at IS NULL`, userID, groupID, at)
	return int(tag.RowsAffected()), err
}

func (s *PostgresStore) EndIDs(ctx context.Context, ids []uuid.UUID, at time.Time) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE management_membership_terms SET ended_at = $2, updated_at = $2
		WHERE id = ANY($1) AND ended_at IS NULL`, ids, at)
	return int(tag.RowsAffected()), err
}

func (s *PostgresStore) Repath(ctx context.Context, groupID uuid.UUID, path string, at time.Time) (int, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE management_membership_terms SET group_path = $2, updated_at = $3
		WHERE group_id = $1 AND ended_at IS NULL AND group_path <> $2`, groupID, path, at)
	return int(tag.RowsAffected()), err
}

func (s *PostgresStore) Date(ctx context.Context, endsAt time.Time, seasonID uuid.UUID, at time.Time) (int, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE management_membership_terms SET ends_at = $1, season_id = $2, updated_at = $3
		WHERE ended_at IS NULL AND ends_at IS NULL`, endsAt, seasonID, at)
	return int(tag.RowsAffected()), err
}

func (s *PostgresStore) Extend(ctx context.Context, id uuid.UUID, endsAt time.Time, seasonID *uuid.UUID, at time.Time) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE management_membership_terms
		SET ends_at = $2, season_id = $3, warned_14d_at = NULL, warned_1d_at = NULL, expired_at = NULL, updated_at = $4
		WHERE id = $1 AND ended_at IS NULL`, id, endsAt, seasonID, at)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *PostgresStore) Warn(ctx context.Context, id uuid.UUID, notice Notice, at time.Time, mails []Mail) (bool, error) {
	var query string
	switch notice {
	case NoticeWarning14d:
		query = `UPDATE management_membership_terms SET warned_14d_at = $2, updated_at = $2
			WHERE id = $1 AND ended_at IS NULL AND expired_at IS NULL AND warned_14d_at IS NULL AND warned_1d_at IS NULL`
	case NoticeWarning1d:
		query = `UPDATE management_membership_terms SET warned_1d_at = $2, warned_14d_at = COALESCE(warned_14d_at, $2), updated_at = $2
			WHERE id = $1 AND ended_at IS NULL AND expired_at IS NULL AND warned_1d_at IS NULL`
	default:
		return false, ErrInvalid
	}
	warned := false
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, query, id, at)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		warned = true
		return enqueue(ctx, tx, mails)
	})
	return warned, err
}

func (s *PostgresStore) Expire(ctx context.Context, ids []uuid.UUID, at time.Time, recipients []uuid.UUID) ([]uuid.UUID, error) {
	var marked []uuid.UUID
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		marked = nil
		rows, err := tx.Query(ctx, `
			UPDATE management_membership_terms SET expired_at = $2, updated_at = $2
			WHERE id = ANY($1) AND ended_at IS NULL AND expired_at IS NULL AND ends_at IS NOT NULL
			RETURNING id`, ids, at)
		if err != nil {
			return err
		}
		marked, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		if err != nil || len(marked) == 0 {
			return err
		}
		mails := make([]Mail, 0, len(recipients))
		for _, r := range recipients {
			mails = append(mails, Mail{RecipientID: r, Notice: NoticeExpiredList, TermIDs: marked, OccurredAt: at})
		}
		return enqueue(ctx, tx, mails)
	})
	return marked, err
}

func (s *PostgresStore) Enqueue(ctx context.Context, mails []Mail) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error { return enqueue(ctx, tx, mails) })
}

func enqueue(ctx context.Context, tx pgx.Tx, mails []Mail) error {
	for _, m := range mails {
		if m.ID == uuid.Nil {
			m.ID = uuid.New()
		}
		var summary []byte
		if m.Summary != nil {
			var err error
			if summary, err = json.Marshal(m.Summary); err != nil {
				return err
			}
		}
		termIDs := m.TermIDs
		if termIDs == nil {
			termIDs = []uuid.UUID{}
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO management_term_mails
				(id, recipient_id, notice, term_id, subject_id, group_id, group_path, ends_at, term_ids, summary, occurred_at, next_attempt_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $11)`,
			m.ID, m.RecipientID, string(m.Notice), m.TermID, m.SubjectID, m.GroupID, m.GroupPath, m.EndsAt, termIDs, summary, m.OccurredAt); err != nil {
			return err
		}
	}
	return nil
}

func (s *PostgresStore) Claim(ctx context.Context, now time.Time, lease time.Duration) (*Mail, error) {
	var m Mail
	var notice string
	var summary []byte
	err := s.pool.QueryRow(ctx, `
		UPDATE management_term_mails SET next_attempt_at = $2, claimed_at = $1
		WHERE id = (
			SELECT id FROM management_term_mails
			WHERE next_attempt_at <= $1
			ORDER BY next_attempt_at, occurred_at, id
			LIMIT 1
			FOR UPDATE SKIP LOCKED)
		RETURNING id, recipient_id, notice, term_id, subject_id, group_id, group_path, ends_at, term_ids, summary,
			occurred_at, next_attempt_at, attempts, claimed_at`, now, now.Add(lease)).
		Scan(&m.ID, &m.RecipientID, &notice, &m.TermID, &m.SubjectID, &m.GroupID, &m.GroupPath, &m.EndsAt, &m.TermIDs, &summary,
			&m.OccurredAt, &m.NextAttemptAt, &m.Attempts, &m.ClaimedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	m.Notice = Notice(notice)
	if summary != nil {
		if err := json.Unmarshal(summary, &m.Summary); err != nil {
			return nil, err
		}
	}
	return &m, nil
}

func (s *PostgresStore) Complete(ctx context.Context, m Mail) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM management_term_mails WHERE id = $1 AND claimed_at = $2`, m.ID, m.ClaimedAt)
	return err
}

func (s *PostgresStore) Retry(ctx context.Context, m Mail, at time.Time) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE management_term_mails SET attempts = attempts + 1, next_attempt_at = $3, claimed_at = NULL
		WHERE id = $1 AND claimed_at = $2`, m.ID, m.ClaimedAt, at)
	return err
}

func (s *PostgresStore) DropBefore(ctx context.Context, before time.Time) (int, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM management_term_mails WHERE occurred_at < $1`, before)
	return int(tag.RowsAffected()), err
}

func (s *PostgresStore) Backlog(ctx context.Context, now time.Time) (int, time.Duration, error) {
	var count int
	var oldest *time.Time
	if err := s.pool.QueryRow(ctx, `SELECT count(*), min(occurred_at) FROM management_term_mails`).Scan(&count, &oldest); err != nil {
		return 0, 0, err
	}
	if oldest == nil {
		return 0, 0, nil
	}
	return count, max(now.Sub(*oldest), 0), nil
}

// lockSQL takes the job's session advisory lock without waiting.
const lockSQL = `SELECT pg_try_advisory_lock(hashtextextended('core-backend management terms', 0))`

func (s *PostgresStore) Lock(ctx context.Context) (func(), bool, error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, false, err
	}
	// Closing the connection ends the session, which releases the lock
	// even when unlocking fails; the pool replaces it.
	release := func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = conn.Hijack().Close(closeCtx)
	}
	var locked bool
	if err := conn.QueryRow(ctx, lockSQL).Scan(&locked); err != nil {
		release()
		return nil, false, err
	}
	if !locked {
		release()
		return nil, false, nil
	}
	return release, true, nil
}

func (s *PostgresStore) LastSuccess(ctx context.Context, mode Mode, trigger Trigger) (*time.Time, error) {
	var last *time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT max(started_at) FROM management_term_runs WHERE mode = $1 AND triggered_by = $2 AND status = 'ok'`,
		string(mode), string(trigger)).Scan(&last)
	return last, err
}

func (s *PostgresStore) LastWeekly(ctx context.Context) (*time.Time, error) {
	var last *time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT max(started_at) FROM management_term_runs WHERE mode = 'apply' AND status = 'ok' AND weekly IN ('sent', 'none')`).Scan(&last)
	return last, err
}

func (s *PostgresStore) StartRun(ctx context.Context, r Run) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			UPDATE management_term_runs SET status = 'abandoned', finished_at = $1, error_code = 'abandoned'
			WHERE status = 'running'`, r.StartedAt); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO management_term_runs (id, mode, triggered_by, started_at, status)
			VALUES ($1, $2, $3, $4, 'running')`, r.ID, string(r.Mode), string(r.Trigger), r.StartedAt)
		return err
	})
}

func (s *PostgresStore) FinishRun(ctx context.Context, r Run) error {
	var code any
	if r.ErrorCode != "" {
		code = r.ErrorCode
	}
	weekly := r.Weekly
	if weekly == "" {
		weekly = WeeklyNotDue
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE management_term_runs SET finished_at = $2, status = $3, error_code = $4, season_found = $5,
			backfilled = $6, dated = $7, ended = $8, warned_14d = $9, warned_1d = $10, expired = $11, mails = $12, weekly = $13
		WHERE id = $1`,
		r.ID, r.FinishedAt, r.Status, code, r.SeasonFound, r.Backfilled, r.Dated, r.Ended, r.Warned14d, r.Warned1d,
		r.Expired, r.Mails, string(weekly))
	return err
}

func (s *PostgresStore) Counts(ctx context.Context, now time.Time) (Counts, error) {
	var c Counts
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*),
			count(*) FILTER (WHERE ends_at IS NULL),
			count(*) FILTER (WHERE expired_at IS NULL AND ends_at > $1 AND ends_at <= $2),
			count(*) FILTER (WHERE expired_at IS NOT NULL OR ends_at <= $1)
		FROM management_membership_terms WHERE ended_at IS NULL`, now, now.Add(WarnEarly)).
		Scan(&c.Open, &c.Undated, &c.Expiring, &c.ExpiredInGroup); err != nil {
		return c, err
	}
	var status *string
	var season *bool
	err := s.pool.QueryRow(ctx, `
		SELECT (SELECT max(started_at) FROM management_term_runs WHERE triggered_by = 'schedule' AND status = 'ok'),
			(SELECT status FROM management_term_runs WHERE triggered_by = 'schedule' AND status <> 'running' ORDER BY started_at DESC LIMIT 1),
			(SELECT season_found FROM management_term_runs WHERE triggered_by = 'schedule' AND status = 'ok' ORDER BY started_at DESC LIMIT 1)`).
		Scan(&c.LastSuccess, &status, &season)
	if err != nil {
		return c, err
	}
	c.LastRunFailed = status != nil && *status != "ok"
	c.SeasonFound = season != nil && *season
	return c, nil
}
