package user

import (
	"context"
	"errors"

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
		SELECT id, email, first_name, last_name, created_at, updated_at
		FROM users WHERE id = $1
	`, id).Scan(&u.ID, &u.Email, &u.FirstName, &u.LastName, &u.CreatedAt, &u.UpdatedAt)
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
		INSERT INTO users (id, email, first_name, last_name)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (id) DO UPDATE SET
			email = excluded.email,
			first_name = excluded.first_name,
			last_name = excluded.last_name,
			updated_at = now()
		RETURNING id, email, first_name, last_name, created_at, updated_at, (xmax = 0)
	`, u.ID, u.Email, u.FirstName, u.LastName).Scan(
		&u.ID, &u.Email, &u.FirstName, &u.LastName, &u.CreatedAt, &u.UpdatedAt, &created,
	)
	if err != nil {
		return User{}, false, err
	}
	return u, created, nil
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
