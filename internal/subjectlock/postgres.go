// Package subjectlock serializes PostgreSQL operations that attach an opaque
// identity subject with creation of that subject's durable deletion marker.
package subjectlock

import (
	"context"
	"errors"
	"sort"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

const ActiveAccountReferenceConstraint = "active_account_reference"

type executor interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

// Lock holds a transaction-scoped advisory lock for id. Every operation that
// can introduce a subject reference without a user foreign key must share this
// lock with account-deletion requests so absence checks remain race-free.
func Lock(ctx context.Context, tx executor, id uuid.UUID) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::text, 6001410475649093715))`, id)
	return err
}

// LockMany acquires subject locks in stable UUID order. Callers that relate
// two identities (for example, a deletion target and its requesting actor)
// must not acquire them in request order because inverse requests could then
// deadlock.
func LockMany(ctx context.Context, tx executor, ids ...uuid.UUID) error {
	unique := make(map[string]uuid.UUID, len(ids))
	keys := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == uuid.Nil {
			continue
		}
		key := id.String()
		if _, ok := unique[key]; ok {
			continue
		}
		unique[key] = id
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if err := Lock(ctx, tx, unique[key]); err != nil {
			return err
		}
	}
	return nil
}

// IsInactiveAccountReference reports the database guard used by all durable
// subject-link writers. Keeping this mapping in one package prevents stores
// from depending on PostgreSQL error-message text.
func IsInactiveAccountReference(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.ConstraintName == ActiveAccountReferenceConstraint
}
