package main

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgxpool"
)

// readinessDatabaseConns are /v1/ready's own connections to the database.
// It pings on one of its own so that a main pool whose connections are all
// busy (a slow moment, not an outage) does not make the task look broken
// and get it restarted under load. One more connection per task in the
// database's budget.
const readinessDatabaseConns = 1

// openReadinessDatabase opens readiness's pool on databaseURL. Nothing
// connects until the first ping.
func openReadinessDatabase(databaseURL string) (*pgxpool.Pool, error) {
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		// pgx's error may quote the address, password and all.
		return nil, errors.New("readiness: the database address cannot be read")
	}
	config.MaxConns = readinessDatabaseConns
	config.MinConns = 0
	return pgxpool.NewWithConfig(context.Background(), config)
}
