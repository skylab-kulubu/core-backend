package migrate

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ApplyBefore is Apply as the release before version runs it: the
// migrations of version and later are not there. It stands in for the older
// image in a test that rolls a migration back.
func ApplyBefore(ctx context.Context, pool *pgxpool.Pool, version int64) error {
	held, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("migrate: connection: %w", err)
	}
	conn := held.Hijack()
	defer conn.Close(context.Background())
	if err := lock(ctx, conn); err != nil {
		return err
	}
	return applyBefore(ctx, conn, version)
}
