package mailpref

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store keeps the explicit choices.
type Store interface {
	// List is every explicit choice of person.
	List(ctx context.Context, person uuid.UUID) ([]Setting, error)
	// Apply writes changes in one transaction: a value replaces the
	// category's row (or makes one), nil deletes it. Writing a person being
	// erased or erased is ErrSubjectInactive.
	Apply(ctx context.Context, person uuid.UUID, changes []Change, source Source, now time.Time) error
	// EraseSubject deletes every row of person (account erasure).
	EraseSubject(ctx context.Context, person uuid.UUID) (int64, error)
	// TeamGroupIDs are the Group ids rows name.
	TeamGroupIDs(ctx context.Context) ([]string, error)
	// DeleteTeams deletes every row of the teams (deleted teams).
	DeleteTeams(ctx context.Context, groupIDs []string) (int64, error)
}

// PostgresStore is the mail_preferences table.
type PostgresStore struct{ pool *pgxpool.Pool }

func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore { return &PostgresStore{pool: pool} }

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func (s *PostgresStore) List(ctx context.Context, person uuid.UUID) ([]Setting, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT scope, coalesce(group_id, ''), coalesce(news_kind, ''), enabled, source, updated_at
		FROM mail_preferences WHERE user_id = $1`, person)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Setting
	for rows.Next() {
		var st Setting
		var scope, kind, source string
		if err := rows.Scan(&scope, &st.Category.GroupID, &kind, &st.Enabled, &source, &st.UpdatedAt); err != nil {
			return nil, err
		}
		st.Category.Scope, st.Category.Kind, st.Source = Scope(scope), Kind(kind), Source(source)
		out = append(out, st)
	}
	return out, rows.Err()
}

func (s *PostgresStore) Apply(ctx context.Context, person uuid.UUID, changes []Change, source Source, now time.Time) error {
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		for _, change := range changes {
			c := change.Category
			if change.Enabled == nil {
				if _, err := tx.Exec(ctx, `
					DELETE FROM mail_preferences
					WHERE user_id = $1 AND scope = $2
					  AND group_id IS NOT DISTINCT FROM $3 AND news_kind IS NOT DISTINCT FROM $4`,
					person, string(c.Scope), nullable(c.GroupID), nullable(string(c.Kind))); err != nil {
					return err
				}
				continue
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO mail_preferences (user_id, scope, group_id, news_kind, enabled, source, updated_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7)
				ON CONFLICT ON CONSTRAINT mail_preferences_key
				DO UPDATE SET enabled = EXCLUDED.enabled, source = EXCLUDED.source, updated_at = EXCLUDED.updated_at`,
				person, string(c.Scope), nullable(c.GroupID), nullable(string(c.Kind)), *change.Enabled, string(source), now); err != nil {
				return err
			}
		}
		return nil
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.ConstraintName == "active_account_reference" {
		return ErrSubjectInactive
	}
	return err
}

func (s *PostgresStore) EraseSubject(ctx context.Context, person uuid.UUID) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM mail_preferences WHERE user_id = $1`, person)
	if err != nil {
		return 0, fmt.Errorf("mail preferences: erase: %w", err)
	}
	return tag.RowsAffected(), nil
}

func (s *PostgresStore) TeamGroupIDs(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT group_id FROM mail_preferences WHERE group_id IS NOT NULL ORDER BY group_id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

func (s *PostgresStore) DeleteTeams(ctx context.Context, groupIDs []string) (int64, error) {
	if len(groupIDs) == 0 {
		return 0, nil
	}
	tag, err := s.pool.Exec(ctx, `DELETE FROM mail_preferences WHERE group_id = ANY($1)`, groupIDs)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// MemoryStore is a Store in memory, for tests and local runs. Inactive
// marks the people the database would refuse.
type MemoryStore struct {
	mu       sync.Mutex
	rows     map[uuid.UUID]map[Category]Setting
	inactive map[uuid.UUID]bool
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{rows: map[uuid.UUID]map[Category]Setting{}, inactive: map[uuid.UUID]bool{}}
}

// MarkInactive makes writes for person fail as for a person being erased.
func (m *MemoryStore) MarkInactive(person uuid.UUID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.inactive[person] = true
}

func (m *MemoryStore) List(_ context.Context, person uuid.UUID) ([]Setting, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Setting
	for _, s := range m.rows[person] {
		out = append(out, s)
	}
	return out, nil
}

func (m *MemoryStore) Apply(_ context.Context, person uuid.UUID, changes []Change, source Source, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.inactive[person] {
		for _, c := range changes {
			if c.Enabled != nil {
				return ErrSubjectInactive
			}
		}
	}
	rows := m.rows[person]
	if rows == nil {
		rows = map[Category]Setting{}
		m.rows[person] = rows
	}
	for _, c := range changes {
		if c.Enabled == nil {
			delete(rows, c.Category)
			continue
		}
		rows[c.Category] = Setting{Category: c.Category, Enabled: *c.Enabled, Source: source, UpdatedAt: now}
	}
	return nil
}

func (m *MemoryStore) EraseSubject(_ context.Context, person uuid.UUID) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := int64(len(m.rows[person]))
	delete(m.rows, person)
	return n, nil
}

func (m *MemoryStore) TeamGroupIDs(context.Context) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, rows := range m.rows {
		for c := range rows {
			if c.GroupID != "" && !slices.Contains(out, c.GroupID) {
				out = append(out, c.GroupID)
			}
		}
	}
	slices.Sort(out)
	return out, nil
}

func (m *MemoryStore) DeleteTeams(_ context.Context, groupIDs []string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for _, rows := range m.rows {
		for c := range rows {
			if slices.Contains(groupIDs, c.GroupID) && c.GroupID != "" {
				delete(rows, c)
				n++
			}
		}
	}
	return n, nil
}
