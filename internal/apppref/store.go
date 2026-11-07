package apppref

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store keeps the preferences.
type Store interface {
	// List is every preference of person in app, by key.
	List(ctx context.Context, person uuid.UUID, app, key string) ([]Item, error)
	// Put writes one value in one transaction: it holds w.Match against the
	// stored version (ErrConflict), counts the app's other keys and bytes
	// (ErrQuota), and stores the value with the version one higher. Writing
	// a person being erased or erased is ErrSubjectInactive.
	Put(ctx context.Context, person uuid.UUID, app, key string, w Write, client string, now time.Time) (Item, error)
	// DeleteKey deletes one key; a missing key is no error.
	DeleteKey(ctx context.Context, person uuid.UUID, app, key string) error
	// DeleteApp deletes every key of person in app and counts them.
	DeleteApp(ctx context.Context, person uuid.UUID, app string) (int64, error)
	// Summaries are person's apps with their key count and last change.
	Summaries(ctx context.Context, person uuid.UUID) ([]Summary, error)
	// EraseSubject deletes every row of person (account erasure) and counts
	// them.
	EraseSubject(ctx context.Context, person uuid.UUID) (int64, error)
}

// PostgresStore is the app_preferences table.
type PostgresStore struct{ pool *pgxpool.Pool }

func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore { return &PostgresStore{pool: pool} }

// List reads one key when key is set, else the app's every key.
func (s *PostgresStore) List(ctx context.Context, person uuid.UUID, app, key string) ([]Item, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT key, value, schema_version, version, updated_at
		FROM app_preferences
		WHERE user_id = $1 AND app = $2 AND ($3 = '' OR key = $3)
		ORDER BY key`, person, app, key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Item
	for rows.Next() {
		var item Item
		var value []byte
		if err := rows.Scan(&item.Key, &value, &item.SchemaVersion, &item.Version, &item.UpdatedAt); err != nil {
			return nil, err
		}
		item.Value = compact(value)
		out = append(out, item)
	}
	return out, rows.Err()
}

// appLockSeed keeps the per person and app lock apart from the subject lock.
const appLockSeed = 8130272044100617241

func (s *PostgresStore) Put(ctx context.Context, person uuid.UUID, app, key string, w Write, client string, now time.Time) (Item, error) {
	var item Item
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// Two writes of the same person and app count the quota one after
		// the other.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::text || '/' || $2, $3))`, person, app, int64(appLockSeed)); err != nil {
			return err
		}
		var current *int64
		var keys, others int
		if err := tx.QueryRow(ctx, `
			SELECT
				(SELECT version FROM app_preferences WHERE user_id = $1 AND app = $2 AND key = $3),
				count(*) FILTER (WHERE key <> $3),
				coalesce(sum(value_bytes) FILTER (WHERE key <> $3), 0)
			FROM app_preferences WHERE user_id = $1 AND app = $2`, person, app, key).Scan(&current, &keys, &others); err != nil {
			return err
		}
		if err := checkWrite(w, current, keys, others); err != nil {
			return err
		}
		var value []byte
		if err := tx.QueryRow(ctx, `
			INSERT INTO app_preferences (user_id, app, key, value, value_bytes, schema_version, version, client_id, updated_at)
			VALUES ($1, $2, $3, $4::jsonb, $5, $6, 1, $7, $8)
			ON CONFLICT ON CONSTRAINT app_preferences_pkey DO UPDATE SET
				value = EXCLUDED.value, value_bytes = EXCLUDED.value_bytes,
				schema_version = EXCLUDED.schema_version, version = app_preferences.version + 1,
				client_id = EXCLUDED.client_id, updated_at = EXCLUDED.updated_at
			RETURNING key, value, schema_version, version, updated_at`,
			person, app, key, string(w.Value), len(w.Value), w.SchemaVersion, client, now).Scan(&item.Key, &value, &item.SchemaVersion, &item.Version, &item.UpdatedAt); err != nil {
			return err
		}
		item.Value = compact(value)
		return nil
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.ConstraintName == "active_account_reference" {
		return Item{}, ErrSubjectInactive
	}
	return item, err
}

func (s *PostgresStore) DeleteKey(ctx context.Context, person uuid.UUID, app, key string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM app_preferences WHERE user_id = $1 AND app = $2 AND key = $3`, person, app, key)
	return err
}

func (s *PostgresStore) DeleteApp(ctx context.Context, person uuid.UUID, app string) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM app_preferences WHERE user_id = $1 AND app = $2`, person, app)
	return tag.RowsAffected(), err
}

func (s *PostgresStore) Summaries(ctx context.Context, person uuid.UUID) ([]Summary, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT app, count(*), max(updated_at)
		FROM app_preferences WHERE user_id = $1
		GROUP BY app ORDER BY app`, person)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Summary
	for rows.Next() {
		var sum Summary
		if err := rows.Scan(&sum.App, &sum.Keys, &sum.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, sum)
	}
	return out, rows.Err()
}

func (s *PostgresStore) EraseSubject(ctx context.Context, person uuid.UUID) (int64, error) {
	var erased int64
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		erased, err = EraseSubjectIn(ctx, tx, person)
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("app preferences: erase: %w", err)
	}
	return erased, nil
}

// EraseSubjectIn is PostgresStore.EraseSubject in tx, a transaction the
// caller owns (a Keycloak deletion event's, docs/integration-events.md).
func EraseSubjectIn(ctx context.Context, tx pgx.Tx, person uuid.UUID) (int64, error) {
	tag, err := tx.Exec(ctx, `DELETE FROM app_preferences WHERE user_id = $1`, person)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// checkWrite holds a write against the stored state: current is the key's
// version (nil when it has none), keys and others the app's other keys and
// their bytes.
func checkWrite(w Write, current *int64, keys, others int) error {
	if w.Match != nil {
		switch {
		case current == nil:
			return ErrConflict
		case !w.Match.Any && *current != w.Match.Version:
			return ErrConflict
		}
	}
	if keys+1 > MaxKeysPerApp || others+len(w.Value) > MaxBytesPerApp {
		return ErrQuota
	}
	return nil
}

// compact is jsonb's text form made compact again, the form the app wrote.
func compact(raw []byte) []byte {
	var out bytes.Buffer
	if err := json.Compact(&out, raw); err != nil {
		return raw
	}
	return out.Bytes()
}

type memoryRow struct {
	item   Item
	client string
}

// MemoryStore is Store in memory, for tests. Inactive marks the people a
// write must refuse.
type MemoryStore struct {
	mu       sync.Mutex
	rows     map[uuid.UUID]map[string]map[string]memoryRow
	inactive map[uuid.UUID]bool
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{rows: map[uuid.UUID]map[string]map[string]memoryRow{}, inactive: map[uuid.UUID]bool{}}
}

// MarkInactive makes every later write for person ErrSubjectInactive, as the
// table's trigger does for a person being erased.
func (m *MemoryStore) MarkInactive(person uuid.UUID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.inactive[person] = true
}

// Client is the client that last wrote the key, "" when there is none.
func (m *MemoryStore) Client(person uuid.UUID, app, key string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.rows[person][app][key].client
}

func (m *MemoryStore) List(_ context.Context, person uuid.UUID, app, key string) ([]Item, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Item
	for k, row := range m.rows[person][app] {
		if key == "" || k == key {
			out = append(out, row.item)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (m *MemoryStore) Put(_ context.Context, person uuid.UUID, app, key string, w Write, client string, now time.Time) (Item, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.inactive[person] {
		return Item{}, ErrSubjectInactive
	}
	keys := m.rows[person][app]
	var current *int64
	if row, ok := keys[key]; ok {
		v := row.item.Version
		current = &v
	}
	count, size := 0, 0
	for k, row := range keys {
		if k != key {
			count++
			size += len(row.item.Value)
		}
	}
	if err := checkWrite(w, current, count, size); err != nil {
		return Item{}, err
	}
	version := int64(1)
	if current != nil {
		version = *current + 1
	}
	item := Item{Key: key, Value: append(json.RawMessage(nil), w.Value...), SchemaVersion: w.SchemaVersion, Version: version, UpdatedAt: now}
	if m.rows[person] == nil {
		m.rows[person] = map[string]map[string]memoryRow{}
	}
	if m.rows[person][app] == nil {
		m.rows[person][app] = map[string]memoryRow{}
	}
	m.rows[person][app][key] = memoryRow{item: item, client: client}
	return item, nil
}

func (m *MemoryStore) DeleteKey(_ context.Context, person uuid.UUID, app, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.rows[person][app], key)
	return nil
}

func (m *MemoryStore) DeleteApp(_ context.Context, person uuid.UUID, app string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := int64(len(m.rows[person][app]))
	delete(m.rows[person], app)
	return n, nil
}

func (m *MemoryStore) Summaries(_ context.Context, person uuid.UUID) ([]Summary, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Summary
	for app, keys := range m.rows[person] {
		if len(keys) == 0 {
			continue
		}
		sum := Summary{App: app, Keys: len(keys)}
		for _, row := range keys {
			if row.item.UpdatedAt.After(sum.UpdatedAt) {
				sum.UpdatedAt = row.item.UpdatedAt
			}
		}
		out = append(out, sum)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].App < out[j].App })
	return out, nil
}

func (m *MemoryStore) EraseSubject(_ context.Context, person uuid.UUID) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for _, keys := range m.rows[person] {
		n += int64(len(keys))
	}
	delete(m.rows, person)
	return n, nil
}
