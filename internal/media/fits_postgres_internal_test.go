package media

import (
	"context"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
)

// The database checks a new Media attachment's purpose again under the
// backfill's lock (media_purpose_fits_role, migration 20260926161000). It
// must answer as the link rules do for every role and purpose.
func TestPostgresTheDatabaseFitsPurposesToRolesLikeTheLinkRules(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	catalogue := reviewedCatalogue()
	for product, roles := range rolePurposes {
		for role := range roles {
			for purpose := range catalogue.purposes {
				var database bool
				if err := pool.QueryRow(ctx, `SELECT media_purpose_fits_role($1, $2, $3)`, product, role, purpose).Scan(&database); err != nil {
					t.Fatal(err)
				}
				if want := fits(product, role, purpose); database != want {
					t.Errorf("%s role %s, purpose %s: the database says %v, the link rules %v", product, role, purpose, database, want)
				}
			}
		}
	}
}
