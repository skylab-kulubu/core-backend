package migrate_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
)

const certificateBatchJustificationVersion = "20261124000000"

// Up, down and up again: a batch written before keeps no justification; the
// check refuses an empty one and one over 500 characters and takes 500.
func TestCertificateBatchJustificationMigration(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	down := readDown(t, certificateBatchJustificationVersion+"_certificate_batch_justification.down.sql")
	if _, err := pool.Exec(ctx, down); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+certificateBatchJustificationVersion); err != nil {
		t.Fatal(err)
	}
	eventID, batchID := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO events (id, name, location, owner_team) VALUES ($1, 'ARTLAB', 'YTÜ', 'WEBLAB')`, eventID); err != nil {
		t.Fatal(err)
	}
	insertBatch := func(id uuid.UUID) error {
		_, err := pool.Exec(ctx, `INSERT INTO certificate_batches (id, event_id, template_version_id, template_source, reason)
			VALUES ($1, $2, '89c0bfe1-5be5-4ddb-8686-4865f51e3401', 'clubDefault', 'manual')`, id, eventID)
		return err
	}
	if err := insertBatch(batchID); err != nil {
		t.Fatal(err)
	}

	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !recorded(t, pool, certificateBatchJustificationVersion) {
		t.Fatal("not recorded")
	}
	var justification *string
	if err := pool.QueryRow(ctx, `SELECT justification FROM certificate_batches WHERE id = $1`, batchID).Scan(&justification); err != nil {
		t.Fatal(err)
	}
	if justification != nil {
		t.Fatalf("existing batch justification %q", *justification)
	}
	set := func(text string) error {
		_, err := pool.Exec(ctx, `UPDATE certificate_batches SET justification = $2 WHERE id = $1`, batchID, text)
		return err
	}
	if err := set(""); err == nil {
		t.Fatal("an empty justification was written")
	}
	if err := set(strings.Repeat("ş", 501)); err == nil {
		t.Fatal("501 characters were written")
	}
	if err := set(strings.Repeat("ş", 500)); err != nil {
		t.Fatalf("500 characters: %v", err)
	}

	if _, err := pool.Exec(ctx, down); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+certificateBatchJustificationVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !recorded(t, pool, certificateBatchJustificationVersion) {
		t.Fatal("not recorded after up again")
	}
	// The fingerprint records a schema that already has the column.
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+certificateBatchJustificationVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !recorded(t, pool, certificateBatchJustificationVersion) {
		t.Fatal("not recorded by its fingerprint")
	}
}
