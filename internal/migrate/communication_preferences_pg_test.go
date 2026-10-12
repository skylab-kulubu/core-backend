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
	communicationPreferencesVersion     = "20261012160000"
	communicationPreferencesStepVersion = "20261012160100"
)

func communicationPreferencesHold(t *testing.T, pool *pgxpool.Pool) (table, step bool) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), `SELECT
		to_regclass('public.communication_preferences') IS NOT NULL,
		EXISTS (
			SELECT 1 FROM pg_constraint
			WHERE conrelid = to_regclass('public.account_deletion_steps')
			  AND conname = 'account_deletion_steps_step_check'
			  AND pg_get_constraintdef(oid) LIKE '%''erase_communication_preferences''%'
		)`).Scan(&table, &step); err != nil {
		t.Fatal(err)
	}
	return table, step
}

// The preference migrations come after every earlier one and keep the
// consent and Wallet steps in the erasure step list.
func TestCommunicationPreferenceMigrationsComeLastAndKeepTheEarlierSteps(t *testing.T) {
	versions, err := migrate.Versions()
	if err != nil {
		t.Fatal(err)
	}
	before := slices.Index(versions, 20261012121000)
	table := slices.Index(versions, 20261012160000)
	step := slices.Index(versions, 20261012160100)
	if before < 0 || table < 0 || step < 0 || !(before < table && table < step) {
		t.Fatalf("versions %v", versions)
	}
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if table, step := communicationPreferencesHold(t, pool); !table || !step {
		t.Fatalf("table=%v step=%v", table, step)
	}
	if consents, step := contactConsentsHold(t, pool); !consents || !step {
		t.Fatalf("the consent step left the list: table=%v step=%v", consents, step)
	}
	if wallet, step := skypassWalletHolds(t, pool); !wallet || !step {
		t.Fatalf("the Wallet step left the list: table=%v step=%v", wallet, step)
	}
	// The fingerprints know the schema: with the versions unrecorded, Apply
	// records them without running the files again (a kept row survives).
	if _, err := pool.Exec(ctx, `INSERT INTO communication_preferences (user_id, scope, enabled, source) VALUES ($1, 'all', false, 'admin')`, uuid.New()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version IN (`+communicationPreferencesVersion+`, `+communicationPreferencesStepVersion+`)`); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var recorded, rows int
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM schema_migrations WHERE version IN (`+communicationPreferencesVersion+`, `+communicationPreferencesStepVersion+`)),
		(SELECT count(*) FROM communication_preferences)`).Scan(&recorded, &rows); err != nil || recorded != 2 || rows != 1 {
		t.Fatalf("recorded=%d rows=%d err=%v", recorded, rows, err)
	}
}

// The table takes only the three shapes of a category, one row per person
// and category (the master switch and club rows have NULLs that still
// collide), and a known source.
func TestCommunicationPreferencesTableKeepsOneRowPerCategory(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	person := uuid.New()
	insert := func(scope string, group, kind any, source string) error {
		_, err := pool.Exec(ctx, `INSERT INTO communication_preferences (user_id, scope, group_id, news_kind, enabled, source)
			VALUES ($1, $2, $3, $4, false, $5)`, person, scope, group, kind, source)
		return err
	}
	for _, ok := range []struct {
		scope       string
		group, kind any
	}{
		{"all", nil, nil}, {"club", nil, "duyuru"}, {"club", nil, "haber"},
		{"team", "g-weblab", "duyuru"}, {"team", "g-weblab", "haber"},
	} {
		if err := insert(ok.scope, ok.group, ok.kind, "account_center"); err != nil {
			t.Fatalf("%v: %v", ok, err)
		}
		if err := insert(ok.scope, ok.group, ok.kind, "unsubscribe_link"); err == nil || !strings.Contains(err.Error(), "communication_preferences_key") {
			t.Fatalf("%v twice: %v", ok, err)
		}
	}
	for _, bad := range []struct {
		scope       string
		group, kind any
		source      string
	}{
		{"all", nil, "duyuru", "admin"},
		{"all", "g", nil, "admin"},
		{"club", nil, nil, "admin"},
		{"club", "g", "haber", "admin"},
		{"team", nil, "haber", "admin"},
		{"team", "", "haber", "admin"},
		{"team", "g-other", nil, "admin"},
		{"team", "g-other", "bulten", "admin"},
		{"everything", nil, nil, "admin"},
		{"team", "g-other", "haber", "self"},
	} {
		if err := insert(bad.scope, bad.group, bad.kind, bad.source); err == nil {
			t.Fatalf("%v accepted", bad)
		}
	}
	// The channel defaults to e-mail, the only one today; a channel that
	// has not shipped is refused until its CHECK widens.
	var channel string
	if err := pool.QueryRow(ctx, `SELECT channel FROM communication_preferences WHERE user_id = $1 AND scope = 'all'`, person).Scan(&channel); err != nil || channel != "email" {
		t.Fatalf("channel = %q %v", channel, err)
	}
	for _, other := range []string{"push", "inbox", "sms", ""} {
		if _, err := pool.Exec(ctx, `INSERT INTO communication_preferences (user_id, channel, scope, enabled, source) VALUES ($1, $2, 'all', false, 'admin')`, uuid.New(), other); err == nil ||
			!strings.Contains(err.Error(), "communication_preferences_channel_check") {
			t.Fatalf("channel %q: %v", other, err)
		}
	}
}

// A person being erased, or erased, gets no new row, so a link opened after
// the erasure does not write their id back. A person without a core row
// (a Member who never signed in to a core app) may still have rows.
func TestCommunicationPreferencesRefuseAnInactiveSubject(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO communication_preferences (user_id, scope, enabled, source) VALUES ($1, 'all', false, 'unsubscribe_link')`, uuid.New()); err != nil {
		t.Fatalf("person without a core row: %v", err)
	}
	store := user.NewPostgresStore(pool)
	holder := uuid.New()
	if _, _, err := user.NewService(store).Ensure(ctx, holder, user.Profile{Email: "pref-holder@example.test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO communication_preferences (user_id, scope, enabled, source) VALUES ($1, 'all', false, 'account_center')`, holder); err != nil {
		t.Fatalf("active person: %v", err)
	}
	if _, err := store.RequestDeletion(ctx, holder, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO communication_preferences (user_id, scope, news_kind, enabled, source) VALUES ($1, 'club', 'haber', false, 'unsubscribe_link')`, holder); err == nil ||
		!strings.Contains(err.Error(), "not active") {
		t.Fatalf("row for a person being erased: %v", err)
	}
}

// The down migrations refuse while they would lose something: a preference
// (dropping it would mail everyone who opted out), or an erasure checkpoint
// of the step. Once nothing is left they roll back, and Apply brings both
// back.
func TestCommunicationPreferenceDownMigrationsRefuseToLoseRecords(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	tableDown := readDown(t, communicationPreferencesVersion+"_communication_preferences.down.sql")
	stepDown := readDown(t, communicationPreferencesStepVersion+"_account_erasure_communication_preferences_step.down.sql")

	store := user.NewPostgresStore(pool)
	holder := uuid.New()
	if _, _, err := user.NewService(store).Ensure(ctx, holder, user.Profile{Email: "pref-down@example.test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO communication_preferences (user_id, scope, enabled, source) VALUES ($1, 'all', false, 'account_center')`, holder); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, tableDown); err == nil || !strings.Contains(err.Error(), "refusing to drop") {
		t.Fatalf("down with a preference: %v", err)
	}
	request, err := store.RequestDeletion(ctx, holder, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO account_deletion_steps (request_id, step) VALUES ($1, 'erase_communication_preferences')`, request.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, stepDown); err == nil || !strings.Contains(err.Error(), "completion proof") {
		t.Fatalf("down with a step checkpoint: %v", err)
	}
	if table, step := communicationPreferencesHold(t, pool); !table || !step {
		t.Fatal("a refused down changed the schema")
	}

	if _, err := pool.Exec(ctx, `DELETE FROM account_deletion_steps WHERE step = 'erase_communication_preferences'`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM communication_preferences`); err != nil {
		t.Fatal(err)
	}
	for _, down := range []string{stepDown, tableDown} {
		if _, err := pool.Exec(ctx, down); err != nil {
			t.Fatalf("down once nothing is left: %v", err)
		}
	}
	if table, step := communicationPreferencesHold(t, pool); table || step {
		t.Fatalf("after down table=%v step=%v", table, step)
	}
	if wallet, step := skypassWalletHolds(t, pool); !wallet || !step {
		t.Fatal("the preference step's down dropped the Wallet step")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version IN (`+communicationPreferencesVersion+`, `+communicationPreferencesStepVersion+`)`); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if table, step := communicationPreferencesHold(t, pool); !table || !step {
		t.Fatal("applying again after down did not bring the preferences back")
	}
}
