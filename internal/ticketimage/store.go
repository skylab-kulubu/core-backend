package ticketimage

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store keeps each Event's Setting (event_ticket_images, migration
// 20261127190000).
type Store interface {
	// Get is the Event's Setting; SourceAuto when it has none.
	Get(ctx context.Context, eventID uuid.UUID) (Setting, error)
	// Put writes it; SourceAuto removes the row.
	Put(ctx context.Context, eventID uuid.UUID, s Setting) (Setting, error)
}

// PostgresStore is the Store in core's database.
type PostgresStore struct {
	Pool *pgxpool.Pool
}

func (s *PostgresStore) Get(ctx context.Context, eventID uuid.UUID) (Setting, error) {
	var out Setting
	var source string
	var crop Crop
	var updated time.Time
	err := s.Pool.QueryRow(ctx, `
		SELECT source, media_id, cover_media_id, crop_x, crop_y, crop_w, crop_h, updated_at
		FROM event_ticket_images WHERE event_id = $1`, eventID).
		Scan(&source, &out.MediaID, &out.CoverMediaID, &crop.X, &crop.Y, &crop.W, &crop.H, &updated)
	if errors.Is(err, pgx.ErrNoRows) {
		return Setting{Source: SourceAuto}, nil
	}
	if err != nil {
		return Setting{}, err
	}
	out.Source, out.Crop, out.UpdatedAt = Source(source), &crop, &updated
	return out, nil
}

func (s *PostgresStore) Put(ctx context.Context, eventID uuid.UUID, in Setting) (Setting, error) {
	if in.Source == SourceAuto || in.Source == "" {
		if _, err := s.Pool.Exec(ctx, `DELETE FROM event_ticket_images WHERE event_id = $1`, eventID); err != nil {
			return Setting{}, err
		}
		return Setting{Source: SourceAuto}, nil
	}
	if in.Crop == nil {
		return Setting{}, ErrInvalid
	}
	c := *in.Crop
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO event_ticket_images (event_id, source, media_id, cover_media_id, crop_x, crop_y, crop_w, crop_h, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, now())
		ON CONFLICT (event_id) DO UPDATE SET
			source = EXCLUDED.source, media_id = EXCLUDED.media_id, cover_media_id = EXCLUDED.cover_media_id,
			crop_x = EXCLUDED.crop_x, crop_y = EXCLUDED.crop_y, crop_w = EXCLUDED.crop_w, crop_h = EXCLUDED.crop_h,
			updated_at = now()`,
		eventID, string(in.Source), in.MediaID, in.CoverMediaID, c.X, c.Y, c.W, c.H)
	if err != nil {
		return Setting{}, err
	}
	return s.Get(ctx, eventID)
}

// MemoryStore is the Store in memory, for tests.
type MemoryStore struct {
	mu   sync.Mutex
	rows map[uuid.UUID]Setting
}

func (m *MemoryStore) Get(_ context.Context, eventID uuid.UUID) (Setting, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.rows[eventID]; ok {
		return s, nil
	}
	return Setting{Source: SourceAuto}, nil
}

func (m *MemoryStore) Put(_ context.Context, eventID uuid.UUID, in Setting) (Setting, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rows == nil {
		m.rows = map[uuid.UUID]Setting{}
	}
	if in.Source == SourceAuto || in.Source == "" {
		delete(m.rows, eventID)
		return Setting{Source: SourceAuto}, nil
	}
	now := time.Now()
	in.UpdatedAt = &now
	m.rows[eventID] = in
	return in, nil
}
