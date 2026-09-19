package migrate_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/certificate"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/eventmail"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/shorturl"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

func TestApplyRepairsBrownfieldSchema(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()

	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `SELECT extra_form_urls FROM events`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `SELECT cover_colors, cover_colors_computed FROM media`); err != nil {
		t.Fatal(err)
	}

	if _, err := pool.Exec(ctx, `ALTER TABLE events DROP COLUMN extra_form_urls`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DROP INDEX url_hits_url_id_at_idx`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DROP TABLE schema_migrations`); err != nil {
		t.Fatal(err)
	}

	_, err := pool.Exec(ctx, `SELECT extra_form_urls FROM events`)
	if err == nil {
		t.Fatal("expected missing column")
	}
	msg := err.Error()
	if !strings.Contains(msg, "extra_form_urls") || !strings.Contains(msg, "42703") {
		t.Fatalf("want SQLSTATE 42703 extra_form_urls, got %v", err)
	}

	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `SELECT extra_form_urls FROM events`); err != nil {
		t.Fatal(err)
	}
	var indexCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_indexes WHERE schemaname = 'public' AND indexname = 'url_hits_url_id_at_idx'`).Scan(&indexCount); err != nil {
		t.Fatal(err)
	}
	if indexCount != 1 {
		t.Fatalf("url_hits index count = %d", indexCount)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
}

func postgresPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	name := fmt.Sprintf("core-migrate-%d", time.Now().UnixNano())
	run := exec.Command("docker", "run", "-d", "--rm", "--name", name,
		"-e", "POSTGRES_PASSWORD=postgres",
		"-e", "POSTGRES_DB=coretest",
		"-p", "127.0.0.1::5432",
		"postgres:17-alpine",
	)
	out, err := run.CombinedOutput()
	if err != nil {
		t.Skipf("docker run postgres: %v %s", err, out)
	}
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", name).Run()
	})

	portOut, err := exec.Command("docker", "port", name, "5432/tcp").CombinedOutput()
	if err != nil {
		t.Fatalf("docker port: %v %s", err, portOut)
	}
	addr := strings.TrimSpace(string(portOut))
	parts := strings.Split(addr, "\n")[0]
	hostport := parts
	if i := strings.LastIndex(parts, "://"); i >= 0 {
		hostport = parts[i+3:]
	}

	url := fmt.Sprintf("postgres://postgres:postgres@%s/coretest?sslmode=disable", hostport)
	deadline := time.Now().Add(30 * time.Second)
	var pool *pgxpool.Pool
	for {
		pool, err = pgxpool.New(context.Background(), url)
		if err == nil {
			err = pool.Ping(context.Background())
		}
		if err == nil {
			t.Cleanup(pool.Close)
			return pool
		}
		if time.Now().After(deadline) {
			t.Fatalf("postgres never became ready: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func TestApplyFreshThenIdempotent(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("no recorded versions")
	}
	if _, err := pool.Exec(ctx, `SELECT extra_form_urls FROM events`); err != nil {
		t.Fatal(err)
	}
	var templateName, scope, scopeKey string
	var rawLayout []byte
	var system bool
	if err := pool.QueryRow(ctx, `SELECT name,system,draft_layout FROM certificate_templates WHERE source_ref='system-default'`).Scan(&templateName, &system, &rawLayout); err != nil {
		t.Fatal(err)
	}
	if templateName != "SKY LAB Varsayılan Sertifika" || !system {
		t.Fatalf("certificate default = %q system=%v", templateName, system)
	}
	var layout certificate.Layout
	if err := json.Unmarshal(rawLayout, &layout); err != nil {
		t.Fatal(err)
	}
	if err := certificate.ValidateLayout(layout); err != nil {
		t.Fatalf("certificate default layout: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT scope,scope_key FROM certificate_template_bindings WHERE scope='club'`).Scan(&scope, &scopeKey); err != nil {
		t.Fatal(err)
	}
	if scope != "club" || scopeKey != "SKY_LAB" {
		t.Fatalf("certificate binding = %q %q", scope, scopeKey)
	}
	if _, err := pool.Exec(ctx, `SELECT asset_manifest FROM certificate_template_versions`); err != nil {
		t.Fatal(err)
	}
	assertDoorStoreQueries(t, pool)
}

func assertDoorStoreQueries(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	ownerID := uuid.New()
	if _, _, err := user.NewService(user.NewPostgresStore(pool)).Ensure(ctx, ownerID, user.Profile{
		Email: "ada@example.com", FirstName: "Ada", LastName: "Lovelace", Username: "ada",
	}); err != nil {
		t.Fatal(err)
	}
	eventStore := event.NewPostgresStore(pool)
	cover, err := media.NewPostgresStore(pool).Create(ctx, media.Media{
		Name:                "cover.png",
		Type:                "image/png",
		Key:                 "images/cover",
		Kind:                media.KindImage,
		UploadedBy:          ownerID,
		CoverColors:         []string{"#3c82be", "#8a642f"},
		CoverColorsComputed: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	ev, err := eventStore.Create(ctx, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB", CoverImageID: &cover.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(ev.CoverColors) != 2 || ev.CoverColors[0] != "#3c82be" {
		t.Fatalf("event cover colors %#v", ev.CoverColors)
	}
	day, err := eventStore.CreateDay(ctx, event.Day{EventID: ev.ID, Name: "Gün 1"})
	if err != nil {
		t.Fatal(err)
	}
	session, err := eventStore.CreateSession(ctx, event.Session{
		EventDayID: day.ID, Title: "Açılış", SessionType: "PRESENTATION",
	})
	if err != nil {
		t.Fatal(err)
	}
	ticketStore := ticket.NewPostgresStore(pool)
	created, err := ticketStore.Create(ctx, ticket.Ticket{
		EventID: ev.ID, TicketType: ticket.Registered, OwnerID: &ownerID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ticketStore.AddCheckIn(ctx, ticket.CheckIn{
		TicketID: created.ID, EventDayID: day.ID, SessionID: session.ID,
	}); err != nil {
		t.Fatal(err)
	}
	search, err := ticketStore.SearchDoorTickets(ctx, ev.ID, "ada", false, 20)
	if err != nil || len(search) != 1 || search[0].Name != "Ada Lovelace" {
		t.Fatalf("search=%+v err=%v", search, err)
	}
	exact, err := ticketStore.SearchDoorTickets(ctx, ev.ID, "ADA@EXAMPLE.COM", true, 2)
	if err != nil || len(exact) != 1 || exact[0].Ticket.ID != created.ID {
		t.Fatalf("exact=%+v err=%v", exact, err)
	}
	owners, err := ticketStore.DoorTicketsByOwners(ctx, ev.ID, []uuid.UUID{ownerID})
	if err != nil || len(owners) != 1 || owners[0].Email != "ada@example.com" {
		t.Fatalf("owners=%+v err=%v", owners, err)
	}
	activity, err := ticketStore.DoorSessionActivity(ctx, session.ID, 20)
	if err != nil || activity.Total != 1 || len(activity.Items) != 1 || activity.Items[0].PersonName != "Ada Lovelace" {
		t.Fatalf("activity=%+v err=%v", activity, err)
	}
	listed, err := ticketStore.ListByEvent(ctx, ev.ID)
	if err != nil || len(listed) != 1 || len(listed[0].CheckIns) != 1 {
		t.Fatalf("listed=%+v err=%v", listed, err)
	}
}

func TestHitRetentionPhysicallyDeletesExpiredPII(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}

	store := shorturl.NewPostgresStore(pool)
	created, err := store.Create(ctx, shorturl.URL{ID: uuid.New(), Alias: "retention-test", URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := store.RecordHit(ctx, created.ID, shorturl.Hit{IP: "192.0.2.1", UserAgent: "old-agent", Referer: "https://old.example", CreatedAt: now.Add(-91 * 24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordHit(ctx, created.ID, shorturl.Hit{IP: "192.0.2.2", UserAgent: "new-agent", Referer: "https://new.example", CreatedAt: now.Add(-89 * 24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}

	if err := store.PruneHits(ctx, now.Add(-shorturl.HitRetention)); err != nil {
		t.Fatal(err)
	}
	var oldRows, newRows, clicks int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM url_hits WHERE ip = '192.0.2.1' OR user_agent = 'old-agent' OR referer = 'https://old.example'`).Scan(&oldRows); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM url_hits WHERE ip = '192.0.2.2'`).Scan(&newRows); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT click_count FROM urls WHERE id = $1`, created.ID).Scan(&clicks); err != nil {
		t.Fatal(err)
	}
	if oldRows != 0 || newRows != 1 || clicks != 1 {
		t.Fatalf("old=%d new=%d clicks=%d", oldRows, newRows, clicks)
	}
}

func TestEventMailSnapshotRetentionStorePersistsExpiry(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}

	store := eventmail.NewPostgresSnapshotStore(pool)
	now := time.Now().UTC().Truncate(time.Microsecond)
	expired := eventmail.Snapshot{MailListID: uuid.New(), EventID: uuid.New(), ExpiresAt: now.Add(-time.Minute)}
	fresh := eventmail.Snapshot{MailListID: uuid.New(), EventID: uuid.New(), ExpiresAt: now.Add(time.Minute)}
	if err := store.Track(ctx, expired); err != nil {
		t.Fatal(err)
	}
	if err := store.Track(ctx, fresh); err != nil {
		t.Fatal(err)
	}

	rows, err := store.Expired(ctx, now, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].MailListID != expired.MailListID || rows[0].EventID != expired.EventID {
		t.Fatalf("expired %+v", rows)
	}
	if err := store.Forget(ctx, expired.MailListID); err != nil {
		t.Fatal(err)
	}
	rows, err = store.Expired(ctx, now.Add(2*time.Minute), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].MailListID != fresh.MailListID {
		t.Fatalf("remaining %+v", rows)
	}
}
