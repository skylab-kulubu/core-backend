package commpref

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
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
	// erased or erased is ErrSubjectInactive. A change of Tracking() also
	// keeps its audit trail (a row when the value changes) and, when it
	// turns tracking off, queues the withdrawal SkyMail must hear of.
	Apply(ctx context.Context, person uuid.UUID, changes []Change, source Source, now time.Time) error
	// EraseSubject deletes every row of person (account erasure): the
	// choices, a waiting tracking withdrawal and the tracking audit trail.
	// It counts the choices.
	EraseSubject(ctx context.Context, person uuid.UUID) (int64, error)
	// Subjects are the people rows name.
	Subjects(ctx context.Context) ([]uuid.UUID, error)
	// TeamGroupIDs are the Group ids rows name.
	TeamGroupIDs(ctx context.Context) ([]string, error)
	// DeleteTeams deletes every row of the teams (deleted teams).
	DeleteTeams(ctx context.Context, groupIDs []string) (int64, error)
	// TeamChoosers are the people who turned kind k on, on channel ch, on
	// any of the teams (a News send's possible followers; an off choice
	// follows nothing).
	TeamChoosers(ctx context.Context, ch Channel, k Kind, groupIDs []string) ([]uuid.UUID, error)
	// ListPeople is every explicit choice of each of people, by person.
	ListPeople(ctx context.Context, people []uuid.UUID) (map[uuid.UUID][]Setting, error)
}

// PostgresStore is the communication_preferences table.
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
		SELECT channel, scope, coalesce(group_id, ''), coalesce(news_kind, ''), enabled, source, updated_at
		FROM communication_preferences WHERE user_id = $1`, person)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Setting
	for rows.Next() {
		var st Setting
		var channel, scope, kind, source string
		if err := rows.Scan(&channel, &scope, &st.Category.GroupID, &kind, &st.Enabled, &source, &st.UpdatedAt); err != nil {
			return nil, err
		}
		st.Channel, st.Category.Scope, st.Category.Kind, st.Source = Channel(channel), Scope(scope), Kind(kind), Source(source)
		out = append(out, st)
	}
	return out, rows.Err()
}

func (s *PostgresStore) Apply(ctx context.Context, person uuid.UUID, changes []Change, source Source, now time.Time) error {
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		for _, change := range changes {
			c := change.Category
			if c.Scope == ScopeTracking {
				if err := applyTracking(ctx, tx, person, change, source, now); err != nil {
					return err
				}
				continue
			}
			if change.Enabled == nil {
				if _, err := tx.Exec(ctx, `
					DELETE FROM communication_preferences
					WHERE user_id = $1 AND channel = $2 AND scope = $3
					  AND group_id IS NOT DISTINCT FROM $4 AND news_kind IS NOT DISTINCT FROM $5`,
					person, string(change.Channel), string(c.Scope), nullable(c.GroupID), nullable(string(c.Kind))); err != nil {
					return err
				}
				continue
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO communication_preferences (user_id, channel, scope, group_id, news_kind, enabled, source, updated_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
				ON CONFLICT ON CONSTRAINT communication_preferences_key
				DO UPDATE SET enabled = EXCLUDED.enabled, source = EXCLUDED.source, updated_at = EXCLUDED.updated_at`,
				person, string(change.Channel), string(c.Scope), nullable(c.GroupID), nullable(string(c.Kind)), *change.Enabled, string(source), now); err != nil {
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
	var erased int64
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		erased, err = EraseSubjectIn(ctx, tx, person)
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("communication preferences: erase: %w", err)
	}
	return erased, nil
}

// EraseSubjectIn is PostgresStore.EraseSubject in tx, a transaction the
// caller commits: an Integration event's handler deletes a person Keycloak
// no longer has with the record of the event.
func EraseSubjectIn(ctx context.Context, tx pgx.Tx, person uuid.UUID) (int64, error) {
	tag, err := tx.Exec(ctx, `DELETE FROM communication_preferences WHERE user_id = $1`, person)
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM communication_tracking_withdrawals WHERE user_id = $1`, person); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM communication_tracking_changes WHERE user_id = $1`, person); err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (s *PostgresStore) Subjects(ctx context.Context) ([]uuid.UUID, error) {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT user_id FROM communication_preferences ORDER BY user_id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}

func (s *PostgresStore) TeamGroupIDs(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT group_id FROM communication_preferences WHERE group_id IS NOT NULL ORDER BY group_id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

func (s *PostgresStore) DeleteTeams(ctx context.Context, groupIDs []string) (int64, error) {
	if len(groupIDs) == 0 {
		return 0, nil
	}
	tag, err := s.pool.Exec(ctx, `DELETE FROM communication_preferences WHERE group_id = ANY($1)`, groupIDs)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (s *PostgresStore) TeamChoosers(ctx context.Context, ch Channel, k Kind, groupIDs []string) ([]uuid.UUID, error) {
	if len(groupIDs) == 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT user_id FROM communication_preferences
		WHERE channel = $1 AND scope = 'team' AND news_kind = $2 AND group_id = ANY($3) AND enabled
		ORDER BY user_id`, string(ch), string(k), groupIDs)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}

func (s *PostgresStore) ListPeople(ctx context.Context, people []uuid.UUID) (map[uuid.UUID][]Setting, error) {
	out := map[uuid.UUID][]Setting{}
	if len(people) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT user_id, channel, scope, coalesce(group_id, ''), coalesce(news_kind, ''), enabled, source, updated_at
		FROM communication_preferences WHERE user_id = ANY($1)`, people)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var person uuid.UUID
		var st Setting
		var channel, scope, kind, source string
		if err := rows.Scan(&person, &channel, &scope, &st.Category.GroupID, &kind, &st.Enabled, &source, &st.UpdatedAt); err != nil {
			return nil, err
		}
		st.Channel, st.Category.Scope, st.Category.Kind, st.Source = Channel(channel), Scope(scope), Kind(kind), Source(source)
		out[person] = append(out[person], st)
	}
	return out, rows.Err()
}

// MemoryStore is a Store in memory, for tests and local runs. Inactive
// marks the people the database would refuse.
type MemoryStore struct {
	mu       sync.Mutex
	rows     map[uuid.UUID]map[memoryKey]Setting
	inactive map[uuid.UUID]bool
	// withdrawals and audit are the tracking outbox and audit trail.
	withdrawals map[uuid.UUID]Withdrawal
	audit       []TrackingChange
}

type memoryKey struct {
	channel  Channel
	category Category
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{rows: map[uuid.UUID]map[memoryKey]Setting{}, inactive: map[uuid.UUID]bool{}, withdrawals: map[uuid.UUID]Withdrawal{}}
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
		rows = map[memoryKey]Setting{}
		m.rows[person] = rows
	}
	for _, c := range changes {
		key := memoryKey{c.Channel, c.Category}
		previous, had := rows[key]
		if c.Enabled == nil {
			delete(rows, key)
			continue
		}
		rows[key] = Setting{Channel: c.Channel, Category: c.Category, Enabled: *c.Enabled, Source: source, UpdatedAt: now}
		if c.Category.Scope != ScopeTracking {
			continue
		}
		if !had || previous.Enabled != *c.Enabled {
			change := TrackingChange{Subject: person, Enabled: *c.Enabled, Source: source, ChangedAt: now}
			if had {
				was := previous.Enabled
				change.Previous = &was
			}
			m.audit = append(m.audit, change)
		}
		if !*c.Enabled {
			m.withdrawals[person] = Withdrawal{Subject: person, RequestedAt: now, NextAttemptAt: now}
		}
	}
	return nil
}

func (m *MemoryStore) EraseSubject(_ context.Context, person uuid.UUID) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := int64(len(m.rows[person]))
	delete(m.rows, person)
	delete(m.withdrawals, person)
	m.audit = slices.DeleteFunc(m.audit, func(c TrackingChange) bool { return c.Subject == person })
	return n, nil
}

func (m *MemoryStore) Subjects(context.Context) ([]uuid.UUID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []uuid.UUID
	for person, rows := range m.rows {
		if len(rows) > 0 {
			out = append(out, person)
		}
	}
	slices.SortFunc(out, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	return out, nil
}

func (m *MemoryStore) TeamGroupIDs(context.Context) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, rows := range m.rows {
		for k := range rows {
			if id := k.category.GroupID; id != "" && !slices.Contains(out, id) {
				out = append(out, id)
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
		for k := range rows {
			if id := k.category.GroupID; id != "" && slices.Contains(groupIDs, id) {
				delete(rows, k)
				n++
			}
		}
	}
	return n, nil
}

func (m *MemoryStore) TeamChoosers(_ context.Context, ch Channel, k Kind, groupIDs []string) ([]uuid.UUID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []uuid.UUID
	for person, rows := range m.rows {
		for key, row := range rows {
			if row.Enabled && key.channel == ch && key.category.Scope == ScopeTeam && key.category.Kind == k && slices.Contains(groupIDs, key.category.GroupID) {
				out = append(out, person)
				break
			}
		}
	}
	slices.SortFunc(out, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	return out, nil
}

func (m *MemoryStore) ListPeople(_ context.Context, people []uuid.UUID) (map[uuid.UUID][]Setting, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[uuid.UUID][]Setting{}
	for _, person := range people {
		for _, s := range m.rows[person] {
			out[person] = append(out[person], s)
		}
	}
	return out, nil
}
