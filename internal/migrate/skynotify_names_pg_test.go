package migrate_test

import (
	"context"
	"io/fs"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/db"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

const (
	skyNotifyNamesVersion    = "20261125210000"
	skyNotifyNamesVersionInt = int64(20261125210000)
)

// The rollback file the window runs by hand is the migration's down and the
// deletion of the version's schema_migrations row, nothing else.
func TestSkyNotifyRollbackFileIsTheDown(t *testing.T) {
	geri, err := fs.ReadFile(db.Tasima, "tasima/skynotify-geri.sql")
	if err != nil {
		t.Fatal(err)
	}
	down := readDown(t, skyNotifyNamesVersion+"_skynotify_names.down.sql")
	rest, found := strings.CutSuffix(string(geri), "\nDELETE FROM schema_migrations WHERE version = "+skyNotifyNamesVersion+";\n")
	if !found {
		t.Fatal("skynotify-geri.sql does not end with the deletion of its schema_migrations row")
	}
	if !strings.HasSuffix(rest, down) {
		t.Fatal("skynotify-geri.sql's body is not the down file: copy the down into it")
	}
	header := strings.TrimSuffix(rest, down)
	for _, line := range strings.Split(strings.TrimSpace(header), "\n") {
		if !strings.HasPrefix(line, "--") {
			t.Fatalf("skynotify-geri.sql runs more than the down: %q", line)
		}
	}
	if user.DeletionStepEraseSkyNotify != "erase_skynotify" {
		t.Fatalf("step %q", user.DeletionStepEraseSkyNotify)
	}
}

// skyNotifyNames is what the rename touches, read back.
type skyNotifyNames struct {
	steps         string // the request's service step rows
	requestCode   string // the request's last_error_code
	otherCode     string // another request's last_error_code, not SkyNotify's
	mailCodes     string // certificate_mails.error_code, by id
	apps          string // app_preferences.app, by key
	clientIDs     string // app_preferences.client_id, by key
	comment       string // team_membership_mails' comment
	checkNamesOld bool   // the step CHECK takes erase_skymail
	checkNamesNew bool   // the step CHECK takes erase_skynotify
}

func readSkyNotifyNames(t *testing.T, pool *pgxpool.Pool, request, other uuid.UUID) skyNotifyNames {
	t.Helper()
	var n skyNotifyNames
	if err := pool.QueryRow(context.Background(), `
		SELECT
			(SELECT string_agg(step || '=' || COALESCE(counts::text, ''), ',' ORDER BY step) FROM account_deletion_steps WHERE request_id = $1),
			(SELECT last_error_code FROM account_deletion_requests WHERE id = $1),
			(SELECT last_error_code FROM account_deletion_requests WHERE id = $2),
			(SELECT string_agg(error_code, ',' ORDER BY created_at, id) FROM certificate_mails),
			(SELECT string_agg(app, ',' ORDER BY key) FROM app_preferences),
			(SELECT string_agg(client_id, ',' ORDER BY key) FROM app_preferences),
			obj_description('team_membership_mails'::regclass, 'pg_class'),
			(SELECT pg_get_constraintdef(oid) LIKE '%''erase_skymail''%' FROM pg_constraint
			 WHERE conrelid = 'account_deletion_steps'::regclass AND conname = 'account_deletion_steps_step_check'),
			(SELECT pg_get_constraintdef(oid) LIKE '%''erase_skynotify''%' FROM pg_constraint
			 WHERE conrelid = 'account_deletion_steps'::regclass AND conname = 'account_deletion_steps_step_check')
	`, request, other).Scan(&n.steps, &n.requestCode, &n.otherCode, &n.mailCodes, &n.apps, &n.clientIDs, &n.comment, &n.checkNamesOld, &n.checkNamesNew); err != nil {
		t.Fatal(err)
	}
	return n
}

// The two-image proof of the SkyNotify rename (ADR 0091; skynotify-ad 04):
// the release before writes the old names; the new image renames them; the
// rollback file gives them back, and the older image starts on that schema
// and writes its step again; the new image then runs the rename once more.
// ApplyBefore stands in for the older image: its migrations are this
// release's without the rename, and it reads no fingerprint of a version
// schema_migrations has.
func TestSkyNotifyNamesTwoImages(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}

	// The older image on a fresh database.
	if err := migrate.ApplyBefore(ctx, pool, skyNotifyNamesVersionInt); err != nil {
		t.Fatal(err)
	}
	if recorded(t, pool, skyNotifyNamesVersion) {
		t.Fatal("the older image recorded the rename")
	}

	// What the older image writes under the old names.
	store := user.NewPostgresStore(pool)
	requestFor := func(email string) uuid.UUID {
		t.Helper()
		subject := uuid.New()
		if _, _, err := user.NewService(store).Ensure(ctx, subject, user.Profile{Email: email}); err != nil {
			t.Fatal(err)
		}
		request, err := store.RequestDeletion(ctx, subject, nil)
		if err != nil {
			t.Fatal(err)
		}
		return request.ID
	}
	request := requestFor("skynotify-rename@example.test")
	other := requestFor("skynotify-rename-other@example.test")
	later := requestFor("skynotify-rename-later@example.test")
	exec(`INSERT INTO account_deletion_steps (request_id, step, counts) VALUES ($1, 'erase_skymail', '{"rows_erased": 1}'), ($1, 'erase_cms', '{"rows": 2}')`, request)
	exec(`UPDATE account_deletion_requests SET last_error_code = 'erase_skymail_rejected_403' WHERE id = $1`, request)
	exec(`UPDATE account_deletion_requests SET last_error_code = 'erase_cms_failed' WHERE id = $1`, other)

	eventID, ticketID, certID := uuid.New(), uuid.New(), uuid.New()
	exec(`INSERT INTO events (id, name, location, owner_team) VALUES ($1, 'ARTLAB', 'YTÜ', 'ARTLAB')`, eventID)
	exec(`INSERT INTO tickets (id, event_id, ticket_type, guest_first_name, guest_last_name, guest_email) VALUES ($1, $2, 'GUEST', 'Ada', 'Lovelace', 'ada@example.com')`, ticketID, eventID)
	exec(`INSERT INTO certificates (id, event_id, ticket_id, serial, recipient_name, recipient_email, event_name, owner_team, verify_url)
		VALUES ($1, $2, $3, 'SERIAL1', 'Ada Lovelace', 'ada@example.com', 'ARTLAB', 'ARTLAB', 'https://skyl.app/c/SERIAL1')`, certID, eventID, ticketID)
	for i, code := range []string{"skymail_503", "skymail_unreachable", "expired"} {
		reason, scope, status := "resend", any("one"), "failed"
		if i == 0 {
			reason, scope, status = "issued", nil, "queued"
		}
		exec(`INSERT INTO certificate_mails (id, certificate_id, event_id, reason, status, error_code, resend_scope, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, now() + make_interval(secs => $8::int))`, uuid.New(), certID, eventID, reason, status, code, scope, i)
	}
	person := uuid.New()
	for _, p := range [][3]string{{"skymail", "a.view", "skymail"}, {"forms", "b.list", "skyforms"}} {
		exec(`INSERT INTO app_preferences (user_id, app, key, value, value_bytes, client_id) VALUES ($1, $2, $3, '{"x":1}', 7, $4)`, person, p[0], p[1], p[2])
	}

	oldNames := skyNotifyNames{
		steps:         `erase_cms={"rows": 2},erase_skymail={"rows_erased": 1}`,
		requestCode:   "erase_skymail_rejected_403",
		otherCode:     "erase_cms_failed",
		mailCodes:     "skymail_503,skymail_unreachable,expired",
		apps:          "skymail,forms",
		clientIDs:     "skymail,skyforms",
		comment:       "Team membership mails waiting for SkyMail (docs/team-membership-mail.md). Ids and a Group path, no address or name; rows live until sent, at most 72 hours.",
		checkNamesOld: true,
	}
	newNames := skyNotifyNames{
		steps:         `erase_cms={"rows": 2},erase_skynotify={"rows_erased": 1}`,
		requestCode:   "erase_skynotify_rejected_403",
		otherCode:     "erase_cms_failed",
		mailCodes:     "skynotify_503,skynotify_unreachable,expired",
		apps:          "skynotify,forms",
		clientIDs:     "skymail,skyforms",
		comment:       "Team membership mails waiting for SkyNotify (docs/team-membership-mail.md). Ids and a Group path, no address or name; rows live until sent, at most 72 hours.",
		checkNamesNew: true,
	}
	if got := readSkyNotifyNames(t, pool, request, other); got != oldNames {
		t.Fatalf("before the rename:\n got %+v\nwant %+v", got, oldNames)
	}

	// The new image renames.
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !recorded(t, pool, skyNotifyNamesVersion) {
		t.Fatal("the rename is not recorded")
	}
	if got := readSkyNotifyNames(t, pool, request, other); got != newNames {
		t.Fatalf("after the rename:\n got %+v\nwant %+v", got, newNames)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO account_deletion_steps (request_id, step) VALUES ($1, 'erase_skymail')`, later); err == nil {
		t.Fatal("the renamed CHECK took erase_skymail")
	}

	// The rollback file, then the older image starts and writes its step.
	geri, err := fs.ReadFile(db.Tasima, "tasima/skynotify-geri.sql")
	if err != nil {
		t.Fatal(err)
	}
	exec(string(geri))
	if recorded(t, pool, skyNotifyNamesVersion) {
		t.Fatal("the rollback left the version in schema_migrations")
	}
	if got := readSkyNotifyNames(t, pool, request, other); got != oldNames {
		t.Fatalf("after the rollback:\n got %+v\nwant %+v", got, oldNames)
	}
	if err := migrate.ApplyBefore(ctx, pool, skyNotifyNamesVersionInt); err != nil {
		t.Fatalf("the older image does not start after the rollback: %v", err)
	}
	if recorded(t, pool, skyNotifyNamesVersion) {
		t.Fatal("the older image recorded the rename")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO account_deletion_steps (request_id, step) VALUES ($1, 'erase_skynotify')`, later); err == nil {
		t.Fatal("the rolled-back CHECK took erase_skynotify")
	}
	exec(`INSERT INTO account_deletion_steps (request_id, step) VALUES ($1, $2)`, later, "erase_skymail")
	exec(`UPDATE account_deletion_requests SET last_error_code = 'erase_skymail_failed' WHERE id = $1`, other)

	// The new image runs the rename again, the older image's rows with it.
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatalf("the new image after the rollback: %v", err)
	}
	if !recorded(t, pool, skyNotifyNamesVersion) {
		t.Fatal("the rename is not recorded again")
	}
	again := newNames
	again.otherCode = "erase_skynotify_failed"
	if got := readSkyNotifyNames(t, pool, request, other); got != again {
		t.Fatalf("after the rename again:\n got %+v\nwant %+v", got, again)
	}
	var laterStep string
	if err := pool.QueryRow(ctx, `SELECT step FROM account_deletion_steps WHERE request_id = $1`, later).Scan(&laterStep); err != nil || laterStep != string(user.DeletionStepEraseSkyNotify) {
		t.Fatalf("the older image's step after the rename: %q %v", laterStep, err)
	}
	var left int
	if err := pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM account_deletion_steps WHERE step = 'erase_skymail')
		     + (SELECT count(*) FROM account_deletion_requests WHERE last_error_code LIKE 'erase\_skymail%')
		     + (SELECT count(*) FROM certificate_mails WHERE error_code LIKE 'skymail\_%')
		     + (SELECT count(*) FROM app_preferences WHERE app = 'skymail')
		     + (SELECT count(*) FROM pg_description WHERE classoid = 'pg_class'::regclass AND description LIKE '%SkyMail%')`).Scan(&left); err != nil || left != 0 {
		t.Fatalf("old names left: %d %v", left, err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatalf("a second start: %v", err)
	}

	// Two rows of one person and key under both names stop either way.
	exec(`INSERT INTO app_preferences (user_id, app, key, value, value_bytes, client_id) VALUES ($1, 'skymail', 'a.view', '{"x":2}', 7, 'skymail')`, person)
	if _, err := pool.Exec(ctx, string(geri)); err == nil || !strings.Contains(err.Error(), "both skynotify and skymail") {
		t.Fatalf("rollback over a collision: %v", err)
	}
}
