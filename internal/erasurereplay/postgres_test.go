package erasurereplay_test

import (
	"context"
	"errors"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/erasurereplay"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// keycloakSnapshotSchema is the part of Keycloak's schema the replay reads,
// with the columns Keycloak 26 has there.
const keycloakSnapshotSchema = `
CREATE TABLE realm (
    id VARCHAR(36) PRIMARY KEY,
    name VARCHAR(255) UNIQUE
);
CREATE TABLE user_entity (
    id VARCHAR(36) PRIMARY KEY,
    email VARCHAR(255),
    email_constraint VARCHAR(255),
    email_verified BOOLEAN NOT NULL DEFAULT false,
    enabled BOOLEAN NOT NULL DEFAULT false,
    first_name VARCHAR(255),
    last_name VARCHAR(255),
    realm_id VARCHAR(255),
    username VARCHAR(255)
);
CREATE TABLE user_attribute (
    name VARCHAR(255) NOT NULL,
    value VARCHAR(255),
    user_id VARCHAR(36) NOT NULL REFERENCES user_entity (id),
    id VARCHAR(36) PRIMARY KEY,
    long_value_hash BYTEA,
    long_value_hash_lower_case BYTEA,
    long_value TEXT
);
INSERT INTO realm (id, name) VALUES ('3a4d7c1e-0000-4000-8000-00000000e5k1', 'e-skylab'), ('master', 'master');
`

// database creates another database on the test server and opens it.
func database(t *testing.T, server *pgxpool.Pool, name string) (*pgxpool.Pool, string) {
	t.Helper()
	ctx := context.Background()
	if _, err := server.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		t.Fatal(err)
	}
	dsn, err := url.Parse(server.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	dsn.Path = "/" + name
	pool, err := pgxpool.New(ctx, dsn.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool, dsn.String()
}

func exec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func keycloakUser(t *testing.T, pool *pgxpool.Pool, realmID string, id uuid.UUID, email *string, attributes map[string][]string) {
	t.Helper()
	exec(t, pool, `INSERT INTO user_entity (id, email, realm_id, username, first_name, last_name) VALUES ($1, $2, $3, $4, 'Ada', 'Lovelace')`,
		id.String(), email, realmID, "u-"+id.String()[:8])
	for name, values := range attributes {
		for _, value := range values {
			if len(value) > 255 {
				exec(t, pool, `INSERT INTO user_attribute (name, value, user_id, id, long_value) VALUES ($1, NULL, $2, $3, $4)`,
					name, id.String(), uuid.NewString(), value)
				continue
			}
			exec(t, pool, `INSERT INTO user_attribute (name, value, user_id, id) VALUES ($1, $2, $3, $4)`,
				name, value, id.String(), uuid.NewString())
		}
	}
}

func TestPostgresReadersReadTheRestoreWindowAndThePeopleInTheSnapshots(t *testing.T) {
	server := testpostgres.Start(t)
	ctx := context.Background()
	restored := time.Date(2026, 9, 20, 3, 0, 0, 0, time.UTC)

	// Live core: requests completed before, at and after T; open requests
	// whose SkyMail or CMS step ran before or after T.
	live := server
	if err := migrate.Apply(ctx, live); err != nil {
		t.Fatal(err)
	}
	subjects := map[string]uuid.UUID{}
	requests := map[string]uuid.UUID{}
	for _, name := range []string{"after", "before", "atT", "openAfter", "openBefore", "openOtherStep"} {
		subjects[name], requests[name] = uuid.New(), uuid.New()
		exec(t, live, `INSERT INTO users (id, email) VALUES ($1, '')`, subjects[name])
	}
	complete := func(name string, at time.Time) {
		exec(t, live, `INSERT INTO account_deletion_requests (id, subject_id, status, completed_at, created_at) VALUES ($1, $2, 'completed', $3, $3)`,
			requests[name], subjects[name], at)
	}
	open := func(name, step string, at time.Time) {
		exec(t, live, `INSERT INTO account_deletion_requests (id, subject_id, status, created_at) VALUES ($1, $2, 'pending', $3)`,
			requests[name], subjects[name], at)
		exec(t, live, `INSERT INTO account_deletion_steps (request_id, step, completed_at) VALUES ($1, $2, $3)`, requests[name], step, at)
	}
	complete("after", restored.Add(time.Hour))
	complete("before", restored.Add(-time.Hour))
	complete("atT", restored)
	open("openAfter", "erase_skymail", restored.Add(time.Minute))
	open("openBefore", "erase_skymail", restored.Add(-time.Minute))
	open("openOtherStep", "erase_cms", restored.Add(time.Minute))

	livePool, err := erasurereplay.OpenReadOnly(ctx, "DATABASE_URL", live.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(livePool.Close)
	liveRequests := erasurereplay.LiveRequests{DB: livePool}
	completed, err := liveRequests.CompletedSince(ctx, restored)
	if err != nil {
		t.Fatal(err)
	}
	want := []erasurereplay.Request{
		{ID: requests["atT"], SubjectID: subjects["atT"]},
		{ID: requests["after"], SubjectID: subjects["after"]},
	}
	if !slices.Equal(completed, want) {
		t.Fatalf("completed since T = %+v, want %+v", completed, want)
	}
	openIDs, err := liveRequests.OpenWithStepSince(ctx, user.DeletionStepEraseSkyMail, restored)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(openIDs, []uuid.UUID{requests["openAfter"]}) {
		t.Fatalf("open since T = %v", openIDs)
	}

	// The read-only connection refuses a write, even to the live database.
	_, err = livePool.Exec(ctx, `UPDATE account_deletion_requests SET last_error_code = 'x'`)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "25006" {
		t.Fatalf("write through the read-only pool = %v", err)
	}
	// Neither a DSN pgx cannot parse nor a database the server does not
	// have shows any part of the DSN: the variable names the database.
	missingDatabase, err := url.Parse(live.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	missingDatabase.Path = "/no_such_database"
	for dsn, want := range map[string]string{
		"postgres://replay:pa55word-never-printed@[::1": "CORE_SNAPSHOT_DATABASE_URL: the DSN cannot be parsed",
		missingDatabase.String():                        "CORE_SNAPSHOT_DATABASE_URL: cannot connect",
	} {
		_, err := erasurereplay.OpenReadOnly(ctx, "CORE_SNAPSHOT_DATABASE_URL", dsn)
		if err == nil || err.Error() != want {
			t.Fatalf("error = %v, want %q", err, want)
		}
	}

	// Core snapshot: a person, and one anonymized before T.
	coreRaw, coreDSN := database(t, server, "snapshot_core")
	if err := migrate.Apply(ctx, coreRaw); err != nil {
		t.Fatal(err)
	}
	person, anonymized, stranger := uuid.New(), uuid.New(), uuid.New()
	exec(t, coreRaw, `INSERT INTO users (id, email, school_email, first_name, last_name) VALUES ($1, ' Ada@Example.com', 'ada.lovelace@std.yildiz.edu.tr', 'Ada', 'Lovelace')`, person)
	exec(t, coreRaw, `INSERT INTO users (id, email, school_email, account_state, anonymized_at) VALUES ($1, '', '', 'anonymized', now())`, anonymized)
	corePool, err := erasurereplay.OpenReadOnly(ctx, "CORE_SNAPSHOT_DATABASE_URL", coreDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(corePool.Close)
	core := erasurereplay.PostgresCoreSnapshot{DB: corePool}
	if err := core.Check(ctx); err != nil {
		t.Fatal(err)
	}
	row, err := core.Get(ctx, person)
	if err != nil || row.Email != " Ada@Example.com" || row.SchoolEmail != "ada.lovelace@std.yildiz.edu.tr" {
		t.Fatalf("core row = %+v, %v", row, err)
	}
	for _, id := range []uuid.UUID{anonymized, stranger} {
		if _, err := core.Get(ctx, id); !errors.Is(err, user.ErrNotFound) {
			t.Fatalf("absent or anonymized core row = %v", err)
		}
	}

	// Keycloak snapshot: the person in the realm, a user with only a long
	// Personal e-mail, and a user of another realm.
	keycloakRaw, keycloakDSN := database(t, server, "snapshot_keycloak")
	exec(t, keycloakRaw, keycloakSnapshotSchema)
	email := "ada@example.com"
	keycloakUser(t, keycloakRaw, "3a4d7c1e-0000-4000-8000-00000000e5k1", person, &email, map[string][]string{
		"schoolEmail":   {"Ada.Lovelace@std.yildiz.edu.tr"},
		"personalEmail": {" ", "ada.personal@example.org"},
		"department":    {"Bilgisayar Mühendisliği"},
	})
	long := strings.Repeat("x", 250) + "@example.org"
	onlyLong, otherRealm := uuid.New(), uuid.New()
	keycloakUser(t, keycloakRaw, "3a4d7c1e-0000-4000-8000-00000000e5k1", onlyLong, nil, map[string][]string{"personalEmail": {long}})
	keycloakUser(t, keycloakRaw, "master", otherRealm, &email, nil)
	keycloakPool, err := erasurereplay.OpenReadOnly(ctx, "KEYCLOAK_SNAPSHOT_DATABASE_URL", keycloakDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(keycloakPool.Close)
	keycloak := erasurereplay.PostgresKeycloakSnapshot{DB: keycloakPool, Realm: "e-skylab"}
	if err := keycloak.Check(ctx); err != nil {
		t.Fatal(err)
	}
	addresses, err := keycloak.UserAddresses(ctx, person)
	if err != nil || !slices.Equal(addresses, []string{"ada@example.com", "Ada.Lovelace@std.yildiz.edu.tr", "ada.personal@example.org"}) {
		t.Fatalf("keycloak addresses = %q, %v", addresses, err)
	}
	if addresses, err := keycloak.UserAddresses(ctx, onlyLong); err != nil || !slices.Equal(addresses, []string{long}) {
		t.Fatalf("long value addresses = %q, %v", addresses, err)
	}
	for _, id := range []uuid.UUID{otherRealm, stranger} {
		if _, err := keycloak.UserAddresses(ctx, id); !errors.Is(err, identity.ErrNotFound) {
			t.Fatalf("user outside the realm = %v", err)
		}
	}
	if err := (erasurereplay.PostgresKeycloakSnapshot{DB: keycloakPool, Realm: "skylab"}).Check(ctx); err == nil {
		t.Fatal("a realm the snapshot does not hold was accepted")
	}
	if err := (erasurereplay.PostgresCoreSnapshot{DB: keycloakPool}).Check(ctx); err == nil {
		t.Fatal("a snapshot without a users table was accepted as core's")
	}
	if err := (erasurereplay.PostgresKeycloakSnapshot{DB: corePool, Realm: "e-skylab"}).Check(ctx); err == nil {
		t.Fatal("a snapshot without Keycloak's tables was accepted as Keycloak's")
	}

	// Together, through the live saga's union: three addresses.
	var records []erasurereplay.Record
	report, err := erasurereplay.Replay{
		Service: skymail, RestoredAt: restored, Requests: &fakeRequests{completed: []erasurereplay.Request{
			{ID: uuid.New(), SubjectID: person}, {ID: uuid.New(), SubjectID: onlyLong}, {ID: uuid.New(), SubjectID: stranger}, {ID: uuid.New(), SubjectID: anonymized},
		}},
		Core: core, Keycloak: keycloak,
		Record: func(record erasurereplay.Record) { records = append(records, record) },
	}.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// The long address is refused by the union (over 254 characters), as
	// the live saga refuses it: that request fails rather than go without it.
	if report.ByAddresses != [4]int{0, 0, 0, 1} || report.Unreadable != 1 || report.Missing != 2 {
		t.Fatalf("report = %+v", report)
	}
}
