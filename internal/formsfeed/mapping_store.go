package formsfeed

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrMappingNotFound is an Event's form with no mapping saved.
var ErrMappingNotFound = errors.New("formsfeed: no form mapping")

// MappingStore keeps the form mappings (PostgresMappings).
type MappingStore interface {
	// Get answers the Event's mapping of the form, or ErrMappingNotFound.
	Get(ctx context.Context, eventID, formID uuid.UUID) (Mapping, error)
	// Put saves the mapping (it replaces one saved before) and answers it
	// as kept; created is true when there was none.
	Put(ctx context.Context, m Mapping) (saved Mapping, created bool, err error)
	// Delete removes the Event's mapping of the form; false when there was
	// none.
	Delete(ctx context.Context, eventID, formID uuid.UUID) (bool, error)
	// ForForm answers every Event's mapping of the form, whether or not
	// the Event still lists it (the caller reads the Events).
	ForForm(ctx context.Context, formID uuid.UUID) ([]Mapping, error)
}

// PostgresMappings is MappingStore on event_form_mappings.
type PostgresMappings struct{ pool *pgxpool.Pool }

// NewPostgresMappings builds the store on pool.
func NewPostgresMappings(pool *pgxpool.Pool) *PostgresMappings { return &PostgresMappings{pool: pool} }

const mappingColumns = `event_id, form_id, COALESCE(first_name_question, ''), COALESCE(last_name_question, ''),
	COALESCE(full_name_question, ''), email_question, updated_by, updated_at`

func scanMapping(row pgx.Row) (Mapping, error) {
	var m Mapping
	err := row.Scan(&m.EventID, &m.FormID, &m.FirstName, &m.LastName, &m.FullName, &m.Email, &m.UpdatedBy, &m.UpdatedAt)
	return m, err
}

func (s *PostgresMappings) Get(ctx context.Context, eventID, formID uuid.UUID) (Mapping, error) {
	m, err := scanMapping(s.pool.QueryRow(ctx, `SELECT `+mappingColumns+` FROM event_form_mappings WHERE event_id = $1 AND form_id = $2`, eventID, formID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Mapping{}, ErrMappingNotFound
	}
	return m, err
}

func (s *PostgresMappings) Put(ctx context.Context, m Mapping) (Mapping, bool, error) {
	m = m.Normalized()
	if m.UpdatedAt.IsZero() {
		m.UpdatedAt = time.Now()
	}
	var created bool
	var saved Mapping
	err := s.pool.QueryRow(ctx, `
		INSERT INTO event_form_mappings AS m
			(event_id, form_id, first_name_question, last_name_question, full_name_question, email_question, updated_by, updated_at)
		VALUES ($1, $2, NULLIF($3, ''), NULLIF($4, ''), NULLIF($5, ''), $6, $7, $8)
		ON CONFLICT (event_id, form_id) DO UPDATE SET
			first_name_question = EXCLUDED.first_name_question, last_name_question = EXCLUDED.last_name_question,
			full_name_question = EXCLUDED.full_name_question, email_question = EXCLUDED.email_question,
			updated_by = EXCLUDED.updated_by, updated_at = EXCLUDED.updated_at
		RETURNING `+mappingColumns+`, (xmax = 0)`,
		m.EventID, m.FormID, m.FirstName, m.LastName, m.FullName, m.Email, m.UpdatedBy, m.UpdatedAt.UTC()).
		Scan(&saved.EventID, &saved.FormID, &saved.FirstName, &saved.LastName, &saved.FullName, &saved.Email, &saved.UpdatedBy, &saved.UpdatedAt, &created)
	return saved, created, err
}

func (s *PostgresMappings) Delete(ctx context.Context, eventID, formID uuid.UUID) (bool, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM event_form_mappings WHERE event_id = $1 AND form_id = $2`, eventID, formID)
	return tag.RowsAffected() > 0, err
}

func (s *PostgresMappings) ForForm(ctx context.Context, formID uuid.UUID) ([]Mapping, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+mappingColumns+` FROM event_form_mappings WHERE form_id = $1 ORDER BY event_id`, formID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Mapping
	for rows.Next() {
		m, err := scanMapping(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
