package media

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
)

// The database checks a new Media attachment's purpose again under the
// backfill's lock, from its copy of the role table (media_role_purposes,
// migration 20260926161000). The copy must hold exactly rolePurposes: no
// pair missing, none extra.
func TestPostgresTheDatabaseRoleTableIsRolePurposes(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	type pair struct {
		Product authz.Product
		Role    Role
		Purpose string
	}
	rows, err := pool.Query(ctx, `SELECT owner_service, role, purpose FROM media_role_purposes()`)
	if err != nil {
		t.Fatal(err)
	}
	database, err := pgx.CollectRows(rows, pgx.RowToStructByPos[pair])
	if err != nil {
		t.Fatal(err)
	}
	var code []pair
	for product, roles := range rolePurposes {
		for role, purposes := range roles {
			for _, purpose := range purposes {
				code = append(code, pair{product, role, purpose})
			}
		}
	}
	order := func(a, b pair) int {
		return strings.Compare(string(a.Product)+" "+string(a.Role)+" "+a.Purpose, string(b.Product)+" "+string(b.Role)+" "+b.Purpose)
	}
	slices.SortFunc(database, order)
	slices.SortFunc(code, order)
	if !slices.Equal(database, code) {
		t.Fatalf("the database's role table\n%v\nis not rolePurposes\n%v", database, code)
	}

	// And the check reads it, with legacy fitting every role.
	for _, c := range []struct {
		product authz.Product
		role    Role
		purpose string
		fits    bool
	}{
		{authz.ProductCMS, RoleCMSImage, PurposeCMSImage, true},
		{authz.ProductCMS, RoleCMSImage, PurposeEventCover, false},
		{authz.ProductCore, RoleCertificateAsset, PurposeLegacy, true},
	} {
		var got bool
		if err := pool.QueryRow(ctx, `SELECT media_purpose_fits_role($1, $2, $3)`, c.product, c.role, c.purpose).Scan(&got); err != nil || got != c.fits {
			t.Errorf("%s %s %s: fits %v (err %v), want %v", c.product, c.role, c.purpose, got, err, c.fits)
		}
	}
}
