package migrate_test

import (
	"context"
	"regexp"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
)

const (
	ticketAccessKeyCodeVersion = "20261122120000"
	ticketAccessKeyCodeDown    = ticketAccessKeyCodeVersion + "_ticket_access_key_code.down.sql"
)

var (
	accessKeyShape = regexp.MustCompile(`^[A-Z2-7]{26}$`)
	codeShape      = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{8}$`)
)

type ticketKeys struct {
	accessKey string
	code      string
	qrVersion int
}

func ticketKeysOf(t *testing.T, pool *pgxpool.Pool) map[uuid.UUID]ticketKeys {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT id, COALESCE(access_key, ''), code, qr_version FROM tickets`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[uuid.UUID]ticketKeys{}
	for rows.Next() {
		var id uuid.UUID
		var k ticketKeys
		if err := rows.Scan(&id, &k.accessKey, &k.code, &k.qrVersion); err != nil {
			t.Fatal(err)
		}
		out[id] = k
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// The migration gives every Ticket already there an access key, a code and
// QR version 1, each key and code its own; running it again changes none of
// them. A Ticket the release before it writes (no new columns in its
// insert) gets them from the column defaults.
func TestTicketAccessKeyCodeMigrationFillsTheTicketsThere(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, readDown(t, ticketAccessKeyCodeDown)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+ticketAccessKeyCodeVersion); err != nil {
		t.Fatal(err)
	}

	eventID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO events (id, name, location, owner_team) VALUES ($1, 'Etkinlik', 'YTÜ', 'WEBLAB')`, eventID); err != nil {
		t.Fatal(err)
	}
	// The insert of the release before: none of the new columns.
	oldInsert := `INSERT INTO tickets (id, event_id, ticket_type, owner_id, guest_first_name, guest_last_name, guest_email, guest_phone_number)
		VALUES ($1, $2, 'GUEST', NULL, 'Ada', 'Lovelace', $3, '')`
	for i := range 50 {
		if _, err := pool.Exec(ctx, oldInsert, uuid.New(), eventID, uuid.NewString()+"@example.com"); err != nil {
			t.Fatalf("ticket %d: %v", i, err)
		}
	}

	// Two Tickets whose holders were erased before: a detached member's and
	// a guest's with the address emptied.
	erasedMember, erasedGuest := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO tickets (id, event_id, ticket_type) VALUES ($1, $3, 'REGISTERED'), ($2, $3, 'GUEST')`,
		erasedMember, erasedGuest, eventID); err != nil {
		t.Fatal(err)
	}

	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	first := ticketKeysOf(t, pool)
	if len(first) != 52 {
		t.Fatalf("%d Tickets", len(first))
	}
	for _, id := range []uuid.UUID{erasedMember, erasedGuest} {
		if k := first[id]; k.accessKey != "" || !codeShape.MatchString(k.code) || k.qrVersion != 1 {
			t.Fatalf("erased ticket %s: %+v", id, k)
		}
		delete(first, id)
	}
	keys, codes := map[string]bool{}, map[string]bool{}
	for id, k := range first {
		if !accessKeyShape.MatchString(k.accessKey) || !codeShape.MatchString(k.code) || k.qrVersion != 1 {
			t.Fatalf("ticket %s: %+v", id, k)
		}
		if keys[k.accessKey] || codes[k.code] {
			t.Fatalf("ticket %s shares a key or code: %+v", id, k)
		}
		keys[k.accessKey], codes[k.code] = true, true
	}

	// Again: nothing changes.
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+ticketAccessKeyCodeVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !recorded(t, pool, ticketAccessKeyCodeVersion) {
		t.Fatal("not recorded")
	}
	second := ticketKeysOf(t, pool)
	if second[erasedMember].accessKey != "" || second[erasedGuest].accessKey != "" {
		t.Fatal("an erased ticket got an access key on the second run")
	}
	for id, k := range first {
		if second[id] != k {
			t.Fatalf("ticket %s changed: %+v → %+v", id, k, second[id])
		}
	}

	// The release before writes on the new schema; the defaults fill it.
	late := uuid.New()
	if _, err := pool.Exec(ctx, oldInsert, late, eventID, "late@example.com"); err != nil {
		t.Fatal(err)
	}
	if k := ticketKeysOf(t, pool)[late]; !accessKeyShape.MatchString(k.accessKey) || !codeShape.MatchString(k.code) || k.qrVersion != 1 {
		t.Fatalf("late ticket %+v", k)
	}
}

// Two Tickets that drew one code before the unique index (forced here) end
// with two codes; the down migration removes the three columns.
func TestTicketAccessKeyCodeMigrationDrawsATakenCodeAgain(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	eventID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO events (id, name, location, owner_team) VALUES ($1, 'Etkinlik', 'YTÜ', 'WEBLAB')`, eventID); err != nil {
		t.Fatal(err)
	}
	for _, email := range []string{"a@example.com", "b@example.com", "c@example.com"} {
		if _, err := pool.Exec(ctx, `INSERT INTO tickets (id, event_id, ticket_type, guest_email) VALUES ($1, $2, 'GUEST', $3)`, uuid.New(), eventID, email); err != nil {
			t.Fatal(err)
		}
	}
	// Back to before the index, with every Ticket on one code.
	if _, err := pool.Exec(ctx, `
		DROP INDEX tickets_code_idx;
		UPDATE tickets SET code = 'K7M2QX9F';
		DELETE FROM schema_migrations WHERE version = `+ticketAccessKeyCodeVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	codes := map[string]bool{}
	for _, k := range ticketKeysOf(t, pool) {
		codes[k.code] = true
	}
	if len(codes) != 3 || !codes["K7M2QX9F"] {
		t.Fatalf("codes %v", codes)
	}

	if _, err := pool.Exec(ctx, readDown(t, ticketAccessKeyCodeDown)); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = 'tickets' AND column_name IN ('access_key', 'code', 'qr_version')`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Fatalf("%d columns left after down", left)
	}
}
