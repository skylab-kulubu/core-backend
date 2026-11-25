package certificate

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/subjectlock"
)

const fontCols = `id, owner_team, family, weight, style, media_id, missing_turkish, uploaded_by, created_at, archived_at`

func scanFont(row pgx.Row) (Font, error) {
	var f Font
	err := row.Scan(&f.ID, &f.OwnerTeam, &f.Family, &f.Weight, &f.Style, &f.MediaID, &f.MissingTurkish, &f.UploadedBy, &f.CreatedAt, &f.ArchivedAt)
	return f, err
}

func collectFonts(rows pgx.Rows, err error) ([]Font, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Font, 0)
	for rows.Next() {
		f, err := scanFont(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (s *PostgresStore) ListFonts(ctx context.Context, teams []string) ([]Font, error) {
	return collectFonts(s.pool.Query(ctx, `SELECT `+fontCols+` FROM certificate_fonts
		WHERE archived_at IS NULL AND owner_team = ANY ($1::text[])
		ORDER BY owner_team, lower(family), weight, style`, teams))
}

func (s *PostgresStore) GetFont(ctx context.Context, id uuid.UUID) (Font, error) {
	f, err := scanFont(s.pool.QueryRow(ctx, `SELECT `+fontCols+` FROM certificate_fonts WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Font{}, ErrNotFound
	}
	return f, err
}

func (s *PostgresStore) FontsByMedia(ctx context.Context, ids []uuid.UUID) ([]Font, error) {
	return collectFonts(s.pool.Query(ctx, `SELECT `+fontCols+` FROM certificate_fonts WHERE media_id = ANY ($1::uuid[]) ORDER BY id`, ids))
}

func (s *PostgresStore) CreateFont(ctx context.Context, f Font) (Font, error) {
	if !validFont(f) {
		return Font{}, ErrInvalid
	}
	created, err := scanFont(s.pool.QueryRow(ctx, `
		INSERT INTO certificate_fonts (id, owner_team, family, weight, style, media_id, missing_turkish, uploaded_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING `+fontCols,
		f.ID, f.OwnerTeam, f.Family, f.Weight, f.Style, f.MediaID, f.MissingTurkish, f.UploadedBy))
	if isUnique(err) {
		// certificate_fonts_unique_face.
		return Font{}, ErrConflict
	}
	if subjectlock.IsInactiveAccountReference(err) {
		return Font{}, ErrForbidden
	}
	if refusal, ok := media.DatabaseLinkRefusal(err); ok {
		return Font{}, refusal
	}
	return created, err
}

func (s *PostgresStore) ArchiveFont(ctx context.Context, id uuid.UUID, at time.Time) (Font, error) {
	f, err := scanFont(s.pool.QueryRow(ctx, `UPDATE certificate_fonts SET archived_at = COALESCE(archived_at, $2)
		WHERE id = $1 RETURNING `+fontCols, id, at))
	if errors.Is(err, pgx.ErrNoRows) {
		return Font{}, ErrNotFound
	}
	return f, err
}

func (s *MemoryStore) ListFonts(_ context.Context, teams []string) ([]Font, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Font, 0)
	for _, f := range s.fonts {
		if f.ArchivedAt != nil {
			continue
		}
		for _, team := range teams {
			if f.OwnerTeam == team {
				out = append(out, f)
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.OwnerTeam != b.OwnerTeam {
			return a.OwnerTeam < b.OwnerTeam
		}
		if fa, fb := strings.ToLower(a.Family), strings.ToLower(b.Family); fa != fb {
			return fa < fb
		}
		if a.Weight != b.Weight {
			return a.Weight < b.Weight
		}
		return a.Style < b.Style
	})
	return out, nil
}

func (s *MemoryStore) GetFont(_ context.Context, id uuid.UUID) (Font, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.fonts[id]
	if !ok {
		return Font{}, ErrNotFound
	}
	return f, nil
}

func (s *MemoryStore) FontsByMedia(_ context.Context, ids []uuid.UUID) ([]Font, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Font, 0)
	for _, f := range s.fonts {
		for _, id := range ids {
			if f.MediaID == id {
				out = append(out, f)
				break
			}
		}
	}
	return out, nil
}

func (s *MemoryStore) CreateFont(_ context.Context, f Font) (Font, error) {
	if !validFont(f) {
		return Font{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fonts == nil {
		s.fonts = make(map[uuid.UUID]Font)
	}
	for _, other := range s.fonts {
		if other.ArchivedAt == nil && other.OwnerTeam == f.OwnerTeam && strings.EqualFold(other.Family, f.Family) &&
			other.Weight == f.Weight && other.Style == f.Style {
			return Font{}, ErrConflict
		}
	}
	f.CreatedAt = time.Now().UTC()
	f.ArchivedAt = nil
	s.fonts[f.ID] = f
	return f, nil
}

func (s *MemoryStore) ArchiveFont(_ context.Context, id uuid.UUID, at time.Time) (Font, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.fonts[id]
	if !ok {
		return Font{}, ErrNotFound
	}
	if f.ArchivedAt == nil {
		f.ArchivedAt = &at
		s.fonts[id] = f
	}
	return f, nil
}
