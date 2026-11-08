package migrate_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

const (
	appPreferencesVersion     = "20261108120000"
	appPreferencesStepVersion = "20261108120100"
)

func appPreferencesHold(t *testing.T, pool *pgxpool.Pool) (table, step bool) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), `SELECT
		to_regclass('public.app_preferences') IS NOT NULL,
		EXISTS (
			SELECT 1 FROM pg_constraint
			WHERE conrelid = to_regclass('public.account_deletion_steps')
			  AND conname = 'account_deletion_steps_step_check'
			  AND pg_get_constraintdef(oid) LIKE '%''erase_app_preferences''%'
		)`).Scan(&table, &step); err != nil {
		t.Fatal(err)
	}
	return table, step
}

// The App preference migrations come after the communication preference
// ones and keep every earlier step in the erasure step list; the
// fingerprints know them.
func TestAppPreferenceMigrationsKeepTheEarlierSteps(t *testing.T) {
	versions, err := migrate.Versions()
	if err != nil {
		t.Fatal(err)
	}
	before := slices.Index(versions, 20261012180100)
	table := slices.Index(versions, 20261108120000)
	step := slices.Index(versions, 20261108120100)
	if before < 0 || table < 0 || step < 0 || !(before < table && table < step) {
		t.Fatalf("versions %v", versions)
	}
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if table, step := appPreferencesHold(t, pool); !table || !step {
		t.Fatalf("table=%v step=%v", table, step)
	}
	if table, step := communicationPreferencesHold(t, pool); !table || !step {
		t.Fatalf("the communication preference step left the list: table=%v step=%v", table, step)
	}
	if consents, step := contactConsentsHold(t, pool); !consents || !step {
		t.Fatalf("the consent step left the list: table=%v step=%v", consents, step)
	}
	if wallet, step := skypassWalletHolds(t, pool); !wallet || !step {
		t.Fatalf("the Wallet step left the list: table=%v step=%v", wallet, step)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO app_preferences (user_id, app, key, value, value_bytes, client_id) VALUES ($1, 'forms', 'forms.list', '{"details":false}', 17, 'skyforms')`, uuid.New()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version IN (`+appPreferencesVersion+`, `+appPreferencesStepVersion+`)`); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var recorded, rows int
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM schema_migrations WHERE version IN (`+appPreferencesVersion+`, `+appPreferencesStepVersion+`)),
		(SELECT count(*) FROM app_preferences)`).Scan(&recorded, &rows); err != nil || recorded != 2 || rows != 1 {
		t.Fatalf("recorded=%d rows=%d err=%v", recorded, rows, err)
	}
}

// The table keeps one row per person, app and key, and refuses names and
// sizes outside the limits.
func TestAppPreferencesTableKeepsItsShape(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	person := uuid.New()
	insert := func(app, key, value string, size, schema int, client string) error {
		_, err := pool.Exec(ctx, `INSERT INTO app_preferences (user_id, app, key, value, value_bytes, schema_version, client_id)
			VALUES ($1, $2, $3, $4::jsonb, $5, $6, $7)`, person, app, key, value, size, schema, client)
		return err
	}
	if err := insert("forms", "forms.list", `{"a":1}`, 7, 1, "skyforms"); err != nil {
		t.Fatal(err)
	}
	if err := insert("forms", "forms.list", `{"a":2}`, 7, 1, "skyforms"); err == nil || !strings.Contains(err.Error(), "app_preferences_pkey") {
		t.Fatalf("same key twice: %v", err)
	}
	for _, bad := range []struct {
		name, app, key, value string
		size, schema          int
		client                string
	}{
		{"app with a capital", "Forms", "k", `1`, 1, 1, "c"},
		{"one-letter app", "f", "k", `1`, 1, 1, "c"},
		{"key with a slash", "forms", "a/b", `1`, 1, 1, "c"},
		{"key starting with a dot", "forms", ".k", `1`, 1, 1, "c"},
		{"no size", "forms", "k", `1`, 0, 1, "c"},
		{"over 4 KB", "forms", "k", `1`, 4097, 1, "c"},
		{"huge value", "forms", "k", `"` + strings.Repeat("a", 16400) + `"`, 4096, 1, "c"},
		{"schema 0", "forms", "k", `1`, 1, 0, "c"},
		{"schema 1001", "forms", "k", `1`, 1, 1001, "c"},
		{"no client", "forms", "k", `1`, 1, 1, ""},
	} {
		if err := insert(bad.app, bad.key, bad.value, bad.size, bad.schema, bad.client); err == nil {
			t.Fatalf("%s accepted", bad.name)
		}
	}
}

// A person being erased, or erased, gets no new row and no rewritten one; a
// person without a core row may have rows.
func TestAppPreferencesRefuseAnInactiveSubject(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	const insert = `INSERT INTO app_preferences (user_id, app, key, value, value_bytes, client_id) VALUES ($1, 'forms', $2, '1', 1, 'skyforms')
		ON CONFLICT ON CONSTRAINT app_preferences_pkey DO UPDATE SET version = app_preferences.version + 1`
	if _, err := pool.Exec(ctx, insert, uuid.New(), "k"); err != nil {
		t.Fatalf("person without a core row: %v", err)
	}
	store := user.NewPostgresStore(pool)
	holder := uuid.New()
	if _, _, err := user.NewService(store).Ensure(ctx, holder, user.Profile{Email: "app-pref-holder@example.test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, insert, holder, "k"); err != nil {
		t.Fatalf("active person: %v", err)
	}
	if _, err := store.RequestDeletion(ctx, holder, nil); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"k", "other"} {
		if _, err := pool.Exec(ctx, insert, holder, key); err == nil || !strings.Contains(err.Error(), "not active") {
			t.Fatalf("%s for a person being erased: %v", key, err)
		}
	}
}

// The down migrations refuse while they would lose something: a preference,
// or an erasure checkpoint of the step. Once nothing is left they roll back
// and keep the communication preference step, and Apply brings both back.
func TestAppPreferenceDownMigrationsRefuseToLoseRecords(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	tableDown := readDown(t, appPreferencesVersion+"_app_preferences.down.sql")
	stepDown := readDown(t, appPreferencesStepVersion+"_account_erasure_app_preferences_step.down.sql")

	store := user.NewPostgresStore(pool)
	holder := uuid.New()
	if _, _, err := user.NewService(store).Ensure(ctx, holder, user.Profile{Email: "app-pref-down@example.test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO app_preferences (user_id, app, key, value, value_bytes, client_id) VALUES ($1, 'forms', 'k', '1', 1, 'skyforms')`, holder); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, tableDown); err == nil || !strings.Contains(err.Error(), "refusing to drop") {
		t.Fatalf("down with a preference: %v", err)
	}
	request, err := store.RequestDeletion(ctx, holder, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO account_deletion_steps (request_id, step) VALUES ($1, 'erase_app_preferences')`, request.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, stepDown); err == nil || !strings.Contains(err.Error(), "completion proof") {
		t.Fatalf("down with a step checkpoint: %v", err)
	}
	if table, step := appPreferencesHold(t, pool); !table || !step {
		t.Fatal("a refused down changed the schema")
	}

	if _, err := pool.Exec(ctx, `DELETE FROM account_deletion_steps WHERE step = 'erase_app_preferences'`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM app_preferences`); err != nil {
		t.Fatal(err)
	}
	for _, down := range []string{stepDown, tableDown} {
		if _, err := pool.Exec(ctx, down); err != nil {
			t.Fatalf("down once nothing is left: %v", err)
		}
	}
	if table, step := appPreferencesHold(t, pool); table || step {
		t.Fatalf("after down table=%v step=%v", table, step)
	}
	if table, step := communicationPreferencesHold(t, pool); !table || !step {
		t.Fatal("the App preference step's down dropped the communication preference step")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version IN (`+appPreferencesVersion+`, `+appPreferencesStepVersion+`)`); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if table, step := appPreferencesHold(t, pool); !table || !step {
		t.Fatalf("after Apply table=%v step=%v", table, step)
	}
}
