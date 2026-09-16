package user

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type PostgresStore struct {
	pool *pgxpool.Pool
}

func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

func (s *PostgresStore) Get(ctx context.Context, id uuid.UUID) (User, error) {
	var u User
	err := s.pool.QueryRow(ctx, `
		SELECT id, email, first_name, last_name, school_email, created_at, updated_at
		FROM users WHERE id = $1
	`, id).Scan(&u.ID, &u.Email, &u.FirstName, &u.LastName, &u.SchoolEmail, &u.CreatedAt, &u.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, err
	}
	return u, nil
}

func (s *PostgresStore) Upsert(ctx context.Context, u User) (User, bool, error) {
	var created bool
	err := s.pool.QueryRow(ctx, `
		INSERT INTO users (id, email, first_name, last_name, school_email)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (id) DO UPDATE SET
			email = excluded.email,
			first_name = excluded.first_name,
			last_name = excluded.last_name,
			school_email = CASE
				WHEN excluded.school_email <> '' THEN excluded.school_email
				ELSE users.school_email
			END,
			updated_at = now()
		RETURNING id, email, first_name, last_name, school_email, created_at, updated_at, (xmax = 0)
	`, u.ID, u.Email, u.FirstName, u.LastName, u.SchoolEmail).Scan(
		&u.ID, &u.Email, &u.FirstName, &u.LastName, &u.SchoolEmail, &u.CreatedAt, &u.UpdatedAt, &created,
	)
	if err != nil {
		return User{}, false, err
	}
	return u, created, nil
}

func (s *PostgresStore) Search(ctx context.Context, q string) ([]User, error) {
	needle := strings.TrimSpace(q)
	rows, err := s.pool.Query(ctx, `
		SELECT id, email, first_name, last_name, school_email, created_at, updated_at
		FROM users
		WHERE lower(email) LIKE '%' || lower($1) || '%'
		   OR lower(school_email) LIKE '%' || lower($1) || '%'
		   OR lower(first_name) LIKE '%' || lower($1) || '%'
		   OR lower(last_name) LIKE '%' || lower($1) || '%'
		   OR lower(first_name || ' ' || last_name) LIKE '%' || lower($1) || '%'
		ORDER BY last_name, first_name, email
	`, needle)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]User, 0)
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Email, &u.FirstName, &u.LastName, &u.SchoolEmail, &u.CreatedAt, &u.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *PostgresStore) Delete(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
