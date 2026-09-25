package shorturl_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/shorturl"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

func TestRecordHitCannotRaceAccountAnonymizationAndRestoreAttribution(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}

	users := user.NewPostgresStore(pool)
	subjectID := uuid.New()
	if _, _, err := user.NewService(users).Ensure(ctx, subjectID, user.Profile{Email: "attribution@example.test"}); err != nil {
		t.Fatal(err)
	}
	urls := shorturl.NewPostgresStore(pool)
	link, err := urls.Create(ctx, shorturl.URL{Alias: "atomic-attribution", URL: "https://example.test"})
	if err != nil {
		t.Fatal(err)
	}

	// Hold the hit table after RecordHit has taken the per-subject lock. The
	// deletion request must wait for that same lock, ensuring its later
	// anonymization observes and scrubs the in-flight hit.
	blocker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(ctx)
	if _, err := blocker.Exec(ctx, `LOCK TABLE url_hits IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}

	hitID := uuid.New()
	hitDone := make(chan error, 1)
	go func() {
		_, err := urls.RecordHit(ctx, link.ID, shorturl.Hit{
			ID: hitID, IP: "192.0.2.44", UserAgent: "Old JWT Browser",
			Referer: "https://private.example.test", UserID: &subjectID,
		})
		hitDone <- err
	}()
	testpostgres.WaitForBlockedQuery(t, pool, "INSERT INTO url_hits")

	deletedAt := time.Date(2026, 9, 20, 15, 0, 0, 0, time.UTC)
	deletionDone := make(chan error, 1)
	go func() {
		if _, err := users.RequestDeletion(ctx, subjectID, nil); err != nil {
			deletionDone <- err
			return
		}
		deletionDone <- users.AnonymizeAccount(ctx, subjectID, deletedAt, nil)
	}()
	testpostgres.WaitForBlockedQuery(t, pool, "pg_advisory_xact_lock")

	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-hitDone; err != nil {
		t.Fatal(err)
	}
	if err := <-deletionDone; err != nil {
		t.Fatal(err)
	}

	var hitUserID *uuid.UUID
	var ip, agent, referer string
	if err := pool.QueryRow(ctx, `SELECT user_id, ip, user_agent, referer FROM url_hits WHERE id=$1`, hitID).Scan(&hitUserID, &ip, &agent, &referer); err != nil {
		t.Fatal(err)
	}
	if hitUserID != nil || ip != "" || agent != "" || referer != "" {
		t.Fatalf("in-flight hit retained erased attribution: user=%v ip=%q agent=%q referer=%q", hitUserID, ip, agent, referer)
	}

	// A bearer that arrives after the durable marker exists is also dropped by
	// the PostgreSQL store itself, even if a caller bypasses the HTTP guard.
	blockedHitID := uuid.New()
	if _, err := urls.RecordHit(ctx, link.ID, shorturl.Hit{ID: blockedHitID, IP: "192.0.2.45", UserID: &subjectID}); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT user_id FROM url_hits WHERE id=$1`, blockedHitID).Scan(&hitUserID); err != nil {
		t.Fatal(err)
	}
	if hitUserID != nil {
		t.Fatalf("blocked bearer was attributed to %s", *hitUserID)
	}
}

func TestRecordHitDropsUnknownSubjectBeforeDatabaseGuard(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	urls := shorturl.NewPostgresStore(pool)
	link, err := urls.Create(ctx, shorturl.URL{Alias: "unknown-attribution", URL: "https://example.test"})
	if err != nil {
		t.Fatal(err)
	}
	unknownID := uuid.New()
	hitID := uuid.New()
	if _, err := urls.RecordHit(ctx, link.ID, shorturl.Hit{ID: hitID, UserID: &unknownID}); err != nil {
		t.Fatal(err)
	}
	var attributed *uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT user_id FROM url_hits WHERE id=$1`, hitID).Scan(&attributed); err != nil {
		t.Fatal(err)
	}
	if attributed != nil {
		t.Fatalf("unknown subject was attributed: %s", *attributed)
	}
	if allowed, err := user.NewPostgresStore(pool).CanAttribute(ctx, unknownID); err != nil || allowed {
		t.Fatalf("unknown subject allowed=%v err=%v", allowed, err)
	}
}

func TestRecordHitAndDisableUseSubjectBeforeURLLockOrder(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	users := user.NewPostgresStore(pool)
	actorID := uuid.New()
	if _, _, err := user.NewService(users).Ensure(ctx, actorID, user.Profile{Email: "url-lock-order@example.test"}); err != nil {
		t.Fatal(err)
	}
	urls := shorturl.NewPostgresStore(pool)
	link, err := urls.Create(ctx, shorturl.URL{Alias: "lock-order", URL: "https://example.test"})
	if err != nil {
		t.Fatal(err)
	}

	// Pause RecordHit after it owns the subject lock but before it can insert
	// the hit and update the URL. Disable must wait for that subject before it
	// touches the URL row. The old URL->subject order formed a cycle as soon as
	// this table lock was released.
	blocker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(ctx)
	if _, err := blocker.Exec(ctx, `LOCK TABLE url_hits IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	hitDone := make(chan error, 1)
	go func() {
		_, err := urls.RecordHit(ctx, link.ID, shorturl.Hit{ID: uuid.New(), UserID: &actorID})
		hitDone <- err
	}()
	testpostgres.WaitForBlockedQuery(t, pool, "INSERT INTO url_hits")

	disableDone := make(chan error, 1)
	go func() { disableDone <- urls.Disable(ctx, link.ID, &actorID) }()
	testpostgres.WaitForBlockedQuery(t, pool, "pg_advisory_xact_lock")

	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-hitDone; err != nil {
		t.Fatalf("record hit: %v", err)
	}
	if err := <-disableDone; err != nil {
		t.Fatalf("disable: %v", err)
	}
	stored, err := urls.GetIncludingDisabled(ctx, link.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.DisabledAt == nil || stored.DisabledBy == nil || *stored.DisabledBy != actorID || stored.ClickCount != 1 {
		t.Fatalf("serialized URL state: %+v", stored)
	}
}
