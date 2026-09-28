package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
)

// replayKeycloakSchema is the part of Keycloak's schema the replay reads.
const replayKeycloakSchema = `
CREATE TABLE realm (id VARCHAR(36) PRIMARY KEY, name VARCHAR(255) UNIQUE);
CREATE TABLE user_entity (
    id VARCHAR(36) PRIMARY KEY, email VARCHAR(255), email_constraint VARCHAR(255),
    enabled BOOLEAN NOT NULL DEFAULT false, first_name VARCHAR(255), last_name VARCHAR(255),
    realm_id VARCHAR(255), username VARCHAR(255)
);
CREATE TABLE user_attribute (
    name VARCHAR(255) NOT NULL, value VARCHAR(255), user_id VARCHAR(36) NOT NULL REFERENCES user_entity (id),
    id VARCHAR(36) PRIMARY KEY, long_value_hash BYTEA, long_value_hash_lower_case BYTEA, long_value TEXT
);
INSERT INTO realm (id, name) VALUES ('0c7e5a52-7d4f-4b1e-9d0a-5f3b2c1d0e9f', 'e-skylab');
`

// replayService stands for the restored SkyMail: its token endpoint on the
// Keycloak side and its erase endpoint, recording what core sends.
type replayService struct {
	mu      sync.Mutex
	tokens  int
	bodies  []string
	paths   []string
	pending map[string]bool
}

func (s *replayService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.Method == http.MethodPost && r.URL.Path == "/realms/e-skylab/protocol/openid-connect/token" {
		form, _ := url.ParseQuery(string(body))
		if form.Get("client_id") != "core-erasure" || form.Get("scope") != "openid account-erase-skymail" {
			http.Error(w, "wrong client", http.StatusBadRequest)
			return
		}
		s.tokens++
		_, _ = w.Write([]byte(`{"access_token":"replay-token","expires_in":300}`))
		return
	}
	requestID, ok := strings.CutPrefix(r.URL.Path, "/internal/v1/account-erasures/")
	if r.Method != http.MethodPut || !ok || r.Header.Get("Authorization") != "Bearer replay-token" {
		http.NotFound(w, r)
		return
	}
	s.bodies = append(s.bodies, string(body))
	s.paths = append(s.paths, r.URL.Path)
	w.Header().Set("Content-Type", "application/json")
	if s.pending[requestID] {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"request_id":"` + requestID + `","status":"in_progress"}`))
		return
	}
	_, _ = w.Write([]byte(`{"request_id":"` + requestID + `","status":"completed","completed_at":"2026-09-27T10:00:00Z","counts":{"recipients_deleted":1}}`))
}

func (s *replayService) sent() (tokens int, bodies, paths []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tokens, append([]string(nil), s.bodies...), append([]string(nil), s.paths...)
}

func (s *replayService) reset(pending ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens, s.bodies, s.paths = 0, nil, nil
	s.pending = map[string]bool{}
	for _, id := range pending {
		s.pending[id] = true
	}
}

func replayDatabase(t *testing.T, server *pgxpool.Pool, name string) (*pgxpool.Pool, string) {
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

func replayExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// The command as the restore wizard runs it: the live core database, the
// core and Keycloak dumps taken with the SkyMail dump at T restored beside
// it, and a SkyMail that answers the Erasure command.
func TestPostgresReplayFromBackupSendsTheSnapshotsAddressesToTheRestoredService(t *testing.T) {
	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previous) })

	server := testpostgres.Start(t)
	ctx := context.Background()
	restored := time.Date(2026, 9, 20, 3, 0, 0, 0, time.UTC)

	// Live core: four people erased, three of them after T. Their rows are
	// anonymized, as after any completed erasure.
	if err := migrate.Apply(ctx, server); err != nil {
		t.Fatal(err)
	}
	type person struct{ subject, request uuid.UUID }
	people := map[string]person{}
	for name, completedAt := range map[string]time.Time{
		"ada": restored.Add(time.Hour), "alan": restored.Add(2 * time.Hour), "grace": restored.Add(3 * time.Hour), "early": restored.Add(-time.Hour),
	} {
		p := person{subject: uuid.New(), request: uuid.New()}
		people[name] = p
		replayExec(t, server, `INSERT INTO users (id, email) VALUES ($1, '')`, p.subject)
		replayExec(t, server, `INSERT INTO account_deletion_requests (id, subject_id, status, created_at, completed_at) VALUES ($1, $2, 'completed', $3, $3)`,
			p.request, p.subject, completedAt)
		replayExec(t, server, `UPDATE users SET account_state = 'anonymized', anonymized_at = $2 WHERE id = $1`, p.subject, completedAt)
	}

	// Core snapshot at T: Ada's row, with her addresses.
	coreSnapshot, coreDSN := replayDatabase(t, server, "core_at_t")
	if err := migrate.Apply(ctx, coreSnapshot); err != nil {
		t.Fatal(err)
	}
	replayExec(t, coreSnapshot, `INSERT INTO users (id, email, school_email, first_name, last_name) VALUES ($1, 'Ada@Example.com', 'ada.lovelace@std.yildiz.edu.tr', 'Ada', 'Lovelace')`,
		people["ada"].subject)
	replayExec(t, coreSnapshot, `INSERT INTO users (id, email, first_name, last_name) VALUES ($1, 'early.bird@example.com', 'Early', 'Bird')`, people["early"].subject)

	// Keycloak snapshot at T: Ada and Alan; Grace is in neither snapshot.
	keycloakSnapshot, keycloakDSN := replayDatabase(t, server, "keycloak_at_t")
	replayExec(t, keycloakSnapshot, replayKeycloakSchema)
	const realmID = "0c7e5a52-7d4f-4b1e-9d0a-5f3b2c1d0e9f"
	keycloakPerson := func(p person, email, first, last string, attributes ...[2]string) {
		replayExec(t, keycloakSnapshot, `INSERT INTO user_entity (id, email, realm_id, username, first_name, last_name) VALUES ($1, $2, $3, $4, $5, $6)`,
			p.subject.String(), email, realmID, strings.ToLower(first), first, last)
		for _, attribute := range attributes {
			replayExec(t, keycloakSnapshot, `INSERT INTO user_attribute (name, value, user_id, id) VALUES ($1, $2, $3, $4)`,
				attribute[0], attribute[1], p.subject.String(), uuid.NewString())
		}
	}
	keycloakPerson(people["ada"], "ada@example.com", "Ada", "Lovelace",
		[2]string{"schoolEmail", "ADA.LOVELACE@std.yildiz.edu.tr"}, [2]string{"personalEmail", "ada.personal@example.org"})
	keycloakPerson(people["alan"], "alan.turing@example.com", "Alan", "Turing")

	service := &replayService{}
	httpServer := httptest.NewServer(service)
	t.Cleanup(httpServer.Close)
	env := map[string]string{
		"DATABASE_URL":                  server.Config().ConnString(),
		"KEYCLOAK_URL":                  httpServer.URL,
		"KEYCLOAK_REALM":                "e-skylab",
		"ACCOUNT_ERASURE_SKYMAIL_URL":   httpServer.URL,
		"ACCOUNT_ERASURE_CLIENT_ID":     "core-erasure",
		"ACCOUNT_ERASURE_CLIENT_SECRET": "s3cr3t-value-never-printed",
		// The snapshots' DSNs carry passwords: environment, never argv.
		"CORE_SNAPSHOT_DATABASE_URL":     coreDSN,
		"KEYCLOAK_SNAPSHOT_DATABASE_URL": keycloakDSN,
	}
	getenv := func(key string) string { return env[key] }
	args := []string{"--service", "skymail", "--restored-at", restored.Format(time.RFC3339)}
	var outputs []string
	run := func(apply bool) (int, string) {
		t.Helper()
		var out bytes.Buffer
		runArgs := args
		if apply {
			runArgs = append(append([]string(nil), args...), "--apply")
		}
		code := runReplayFromBackup(runArgs, getenv, &out)
		outputs = append(outputs, out.String())
		return code, out.String()
	}
	missingLine := "request_id=" + people["grace"].request.String() + " outcome=fail code=subject_missing"

	// Dry run: counts only, the missing subject as a FAIL, nothing sent.
	service.reset()
	code, out := run(false)
	if code != 1 {
		t.Fatalf("dry run exit %d\n%s", code, out)
	}
	for _, line := range []string{
		"dry run, nothing is sent",
		"requests completed at or after the restore: 3\n",
		"with 0 address(es) resolved: 0\n", "with 1 address(es) resolved: 1\n",
		"with 2 address(es) resolved: 0\n", "with 3 address(es) resolved: 1\n",
		"subject in neither snapshot (FAIL): 1\n", "addresses unreadable (FAIL): 0\n",
		missingLine,
		"FAIL: 1 request(s) not replayed",
	} {
		if !strings.Contains(out, line) {
			t.Fatalf("dry run output lacks %q:\n%s", line, out)
		}
	}
	if strings.Contains(out, "outcome=resolved") || strings.Contains(out, people["ada"].request.String()) {
		t.Fatalf("a dry run printed more than counts and FAILs:\n%s", out)
	}
	if tokens, bodies, _ := service.sent(); tokens != 0 || len(bodies) != 0 {
		t.Fatalf("a dry run asked for %d tokens and sent %d commands", tokens, len(bodies))
	}

	// Apply: exactly the normal Erasure command, with the union of both
	// snapshots, to the restored SkyMail; the missing subject still fails.
	service.reset()
	code, out = run(true)
	if code != 1 {
		t.Fatalf("apply exit %d\n%s", code, out)
	}
	// The body the erasure client sends, byte for byte.
	command := func(p person, emails ...string) string {
		list, err := json.Marshal(emails)
		if err != nil {
			t.Fatal(err)
		}
		return `{"request_id":"` + p.request.String() + `","subject_id":"` + p.subject.String() + `","emails":` + string(list) + `}`
	}
	wantBodies := []string{
		command(people["ada"], "ada@example.com", "ada.lovelace@std.yildiz.edu.tr", "ada.personal@example.org"),
		command(people["alan"], "alan.turing@example.com"),
	}
	_, bodies, paths := service.sent()
	if strings.Join(bodies, "\n") != strings.Join(wantBodies, "\n") {
		t.Fatalf("sent bodies:\n%s\nwant:\n%s", strings.Join(bodies, "\n"), strings.Join(wantBodies, "\n"))
	}
	if paths[0] != "/internal/v1/account-erasures/"+people["ada"].request.String() {
		t.Fatalf("path = %s", paths[0])
	}
	for _, line := range []string{
		"request_id=" + people["ada"].request.String() + " outcome=done counts=recipients_deleted:1",
		"request_id=" + people["alan"].request.String() + " outcome=done counts=recipients_deleted:1",
		missingLine, "done (200): 2\n", "failed at the service (FAIL): 0\n",
	} {
		if !strings.Contains(out, line) {
			t.Fatalf("apply output lacks %q:\n%s", line, out)
		}
	}

	// Grace turns up in a Keycloak snapshot; SkyMail is still at work on
	// Alan: nothing failed, but the replay is not finished.
	keycloakPerson(people["grace"], "grace.hopper@example.com", "Grace", "Hopper")
	service.reset(people["alan"].request.String())
	code, out = run(true)
	if code != replayExitRetryLater || !strings.Contains(out, "request_id="+people["alan"].request.String()+" outcome=retry_later code=in_progress") ||
		!strings.Contains(out, "done (200): 2\n") {
		t.Fatalf("202 run: exit %d\n%s", code, out)
	}

	// Everything done: exit 0.
	service.reset()
	if code, out = run(true); code != 0 || !strings.Contains(out, "done (200): 3\n") {
		t.Fatalf("clean run: exit %d\n%s", code, out)
	}

	// A snapshot that cannot be reached, or names a database the server does
	// not have, is named by its variable alone: pgx's connect error, which
	// names the user, host and database, is not printed.
	unreachable, err := url.Parse(coreDSN)
	if err != nil {
		t.Fatal(err)
	}
	unreachable.Host = "127.0.0.1:1"
	env["CORE_SNAPSHOT_DATABASE_URL"] = unreachable.String()
	service.reset()
	if code, out = run(true); code != 1 || !strings.Contains(out, "CORE_SNAPSHOT_DATABASE_URL: cannot connect\n") {
		t.Fatalf("unreachable core snapshot: exit %d\n%s", code, out)
	}
	noDatabase, err := url.Parse(keycloakDSN)
	if err != nil {
		t.Fatal(err)
	}
	noDatabase.Path = "/missing_db"
	env["CORE_SNAPSHOT_DATABASE_URL"], env["KEYCLOAK_SNAPSHOT_DATABASE_URL"] = coreDSN, noDatabase.String()
	if code, out = run(true); code != 1 || !strings.Contains(out, "KEYCLOAK_SNAPSHOT_DATABASE_URL: cannot connect\n") {
		t.Fatalf("keycloak snapshot without its database: exit %d\n%s", code, out)
	}
	if tokens, bodies, _ := service.sent(); tokens != 0 || len(bodies) != 0 {
		t.Fatalf("a run that could not open its snapshots asked for %d tokens and sent %d commands", tokens, len(bodies))
	}

	// No address, name, subject id, DSN password or secret in anything the
	// command printed or logged.
	everything := strings.ToLower(strings.Join(outputs, "\n") + logs.String())
	serverAddress, err := url.Parse(server.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	forbidden := []string{"@example.com", "@example.org", "yildiz.edu.tr", "lovelace", "turing", "hopper", "early.bird",
		"postgres:postgres", "s3cr3t", "replay-token",
		serverAddress.Host, "127.0.0.1", "user=postgres", "coretest", "core_at_t", "keycloak_at_t", "missing_db"}
	for _, p := range people {
		forbidden = append(forbidden, p.subject.String())
	}
	for _, value := range forbidden {
		if strings.Contains(everything, strings.ToLower(value)) {
			t.Fatalf("output or logs carry %q:\n%s", value, everything)
		}
	}
}
