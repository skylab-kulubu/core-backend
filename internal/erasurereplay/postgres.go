package erasurereplay

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// Querier is the part of a pgx pool the readers use.
type Querier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

// OpenReadOnly opens a small pool whose every transaction is read-only
// (`default_transaction_read_only`) and refuses a server that does not
// report the setting on. what names the database in errors; no error carries
// the DSN.
func OpenReadOnly(ctx context.Context, what, dsn string) (*pgxpool.Pool, error) {
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("%s: the DSN cannot be parsed", what)
	}
	config.MaxConns = 2
	config.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
	config.ConnConfig.RuntimeParams["application_name"] = "core-backend replay-from-backup"
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("%s: cannot open the connection pool", what)
	}
	var readOnly string
	if err := pool.QueryRow(ctx, `SHOW default_transaction_read_only`).Scan(&readOnly); err != nil {
		pool.Close()
		return nil, fmt.Errorf("%s: cannot connect", what)
	}
	if readOnly != "on" {
		pool.Close()
		return nil, fmt.Errorf("%s: the connection is not read-only", what)
	}
	return pool, nil
}

// LiveRequests reads the deletion requests from the live core database.
type LiveRequests struct {
	DB Querier
}

func (l LiveRequests) CompletedSince(ctx context.Context, t time.Time) ([]Request, error) {
	rows, err := l.DB.Query(ctx, `
		SELECT id, subject_id
		FROM account_deletion_requests
		WHERE status = 'completed' AND completed_at >= $1
		ORDER BY completed_at, id
	`, t)
	if err != nil {
		return nil, errors.New("live core: completed requests could not be read")
	}
	requests, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Request, error) {
		var request Request
		err := row.Scan(&request.ID, &request.SubjectID)
		return request, err
	})
	if err != nil {
		return nil, errors.New("live core: completed requests could not be read")
	}
	return requests, nil
}

func (l LiveRequests) OpenWithStepSince(ctx context.Context, step user.DeletionStep, t time.Time) ([]uuid.UUID, error) {
	rows, err := l.DB.Query(ctx, `
		SELECT request.id
		FROM account_deletion_requests request
		JOIN account_deletion_steps step ON step.request_id = request.id AND step.step = $1
		WHERE request.status <> 'completed' AND step.completed_at >= $2
		ORDER BY request.created_at, request.id
	`, string(step), t)
	if err != nil {
		return nil, errors.New("live core: open requests could not be read")
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, errors.New("live core: open requests could not be read")
	}
	return ids, nil
}

// PostgresCoreSnapshot reads `users` rows from a core snapshot.
type PostgresCoreSnapshot struct {
	DB Querier
}

func (c PostgresCoreSnapshot) Check(ctx context.Context) error {
	var people bool
	if err := c.DB.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users)`).Scan(&people); err != nil {
		return errors.New("core snapshot: no users table could be read")
	}
	if !people {
		return errors.New("core snapshot: the users table is empty")
	}
	return nil
}

// Get returns the person's primary and school e-mail. An anonymized row holds
// nothing of the person and counts as absent.
func (c PostgresCoreSnapshot) Get(ctx context.Context, id uuid.UUID) (user.User, error) {
	row := user.User{ID: id}
	err := c.DB.QueryRow(ctx, `
		SELECT email, school_email FROM users WHERE id = $1 AND account_state <> 'anonymized'
	`, id).Scan(&row.Email, &row.SchoolEmail)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return user.User{}, user.ErrNotFound
	case err != nil:
		return user.User{}, errors.New("core snapshot: users row could not be read")
	}
	return row, nil
}

// erasureAddressAttributes are the Keycloak user attributes that hold an
// address besides `email`, in the order the live identity adapter reads them.
var erasureAddressAttributes = []string{"schoolEmail", "personalEmail"}

// PostgresKeycloakSnapshot reads users of one realm from a snapshot of
// Keycloak's database: `realm`, `user_entity` and `user_attribute`.
type PostgresKeycloakSnapshot struct {
	DB    Querier
	Realm string
}

func (k PostgresKeycloakSnapshot) Check(ctx context.Context) error {
	var people bool
	err := k.DB.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM user_entity person JOIN realm ON realm.id = person.realm_id WHERE realm.name = $1
		)
	`, k.Realm).Scan(&people)
	if err != nil {
		return errors.New("keycloak snapshot: no realm and user tables could be read")
	}
	if !people {
		return fmt.Errorf("keycloak snapshot: realm %q holds no user", k.Realm)
	}
	return nil
}

// UserAddresses returns the user's `email` and `schoolEmail` and
// `personalEmail` values, raw and without blanks, as the live identity
// adapter does. A value too long for Keycloak's `value` column is read from
// `long_value`. A user the realm does not hold is identity.ErrNotFound.
func (k PostgresKeycloakSnapshot) UserAddresses(ctx context.Context, id uuid.UUID) ([]string, error) {
	var email *string
	err := k.DB.QueryRow(ctx, `
		SELECT person.email
		FROM user_entity person JOIN realm ON realm.id = person.realm_id
		WHERE realm.name = $1 AND person.id = $2
	`, k.Realm, id.String()).Scan(&email)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, identity.ErrNotFound
	case err != nil:
		return nil, errors.New("keycloak snapshot: user could not be read")
	}
	var addresses []string
	if email != nil {
		addresses = append(addresses, *email)
	}
	rows, err := k.DB.Query(ctx, `
		SELECT COALESCE(value, long_value, '')
		FROM user_attribute
		WHERE user_id = $1 AND name = ANY($2::text[])
		ORDER BY array_position($2::text[], name::text), 1
	`, id.String(), erasureAddressAttributes)
	if err != nil {
		return nil, errors.New("keycloak snapshot: user attributes could not be read")
	}
	values, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, errors.New("keycloak snapshot: user attributes could not be read")
	}
	addresses = append(addresses, values...)
	// Blanks go here as in the live adapter; the union drops them too.
	kept := addresses[:0]
	for _, address := range addresses {
		if strings.TrimSpace(address) != "" {
			kept = append(kept, address)
		}
	}
	return kept, nil
}
