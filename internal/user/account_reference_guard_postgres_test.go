package user_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/competitor"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

func TestSelfRequestedDeletionPassesActorGuardAtomically(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	users := user.NewPostgresStore(pool)
	subjectID := uuid.New()
	if _, _, err := user.NewService(users).Ensure(ctx, subjectID, user.Profile{Email: "self-delete@example.test"}); err != nil {
		t.Fatal(err)
	}
	request, err := users.RequestDeletion(ctx, subjectID, &subjectID)
	if err != nil {
		t.Fatal(err)
	}
	if request.RequestedBy == nil || *request.RequestedBy != subjectID {
		t.Fatalf("self requester = %v", request.RequestedBy)
	}
	repeated, err := users.RequestDeletion(ctx, subjectID, &subjectID)
	if err != nil {
		t.Fatal(err)
	}
	if repeated.ID != request.ID {
		t.Fatalf("self request was not idempotent: %s != %s", repeated.ID, request.ID)
	}
	var state user.AccountState
	var requests, outbox int
	if err := pool.QueryRow(ctx, `
		SELECT account_state,
		       (SELECT count(*) FROM account_deletion_requests WHERE subject_id=$1),
		       (SELECT count(*) FROM account_deletion_outbox WHERE subject_id=$1)
		FROM users WHERE id=$1
	`, subjectID).Scan(&state, &requests, &outbox); err != nil {
		t.Fatal(err)
	}
	if state != user.AccountDeletionPending || requests != 1 || outbox != 1 {
		t.Fatalf("state=%s requests=%d outbox=%d", state, requests, outbox)
	}
}

func TestDeletionMarkerRejectsNewSubjectLinksAndDetachedCompetitorReinstate(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	users := user.NewPostgresStore(pool)
	subjectID := uuid.New()
	if _, _, err := user.NewService(users).Ensure(ctx, subjectID, user.Profile{Email: "reference-guard@example.test"}); err != nil {
		t.Fatal(err)
	}
	eventRow, err := event.NewPostgresStore(pool).Create(ctx, event.Event{
		Name: "Reference guard", Location: "YTÜ", OwnerTeam: "WEBLAB",
	})
	if err != nil {
		t.Fatal(err)
	}
	competitors := competitor.NewPostgresStore(pool)
	existing, err := competitors.Create(ctx, competitor.Competitor{UserID: subjectID, EventID: eventRow.ID})
	if err != nil {
		t.Fatal(err)
	}
	request, err := users.RequestDeletion(ctx, subjectID, &subjectID)
	if err != nil {
		t.Fatal(err)
	}
	if request.RequestedBy == nil || *request.RequestedBy != subjectID {
		t.Fatalf("self-deletion requester = %v", request.RequestedBy)
	}

	if _, err := ticket.NewPostgresStore(pool).Create(ctx, ticket.Ticket{
		EventID: eventRow.ID, TicketType: ticket.Registered, OwnerID: &subjectID,
	}); !errors.Is(err, ticket.ErrNotFound) {
		t.Fatalf("ticket create error = %v", err)
	}
	if _, err := competitors.Create(ctx, competitor.Competitor{
		UserID: subjectID, EventID: eventRow.ID,
	}); !errors.Is(err, competitor.ErrConflict) {
		t.Fatalf("competitor create error = %v", err)
	}
	if _, err := media.NewPostgresStore(pool).Create(ctx, media.Media{
		Name: "blocked.png", Type: "image/png", Key: "images/blocked",
		Kind: media.KindImage, UploadedBy: subjectID,
	}); !errors.Is(err, media.ErrForbidden) {
		t.Fatalf("media create error = %v", err)
	}
	targetID := uuid.New()
	if _, _, err := user.NewService(users).Ensure(ctx, targetID, user.Profile{Email: "requested-by-target@example.test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := users.RequestDeletion(ctx, targetID, &subjectID); !errors.Is(err, user.ErrAccountBlocked) {
		t.Fatalf("deletion request with inactive requester error = %v", err)
	}
	var targetState user.AccountState
	var targetMarkers int
	if err := pool.QueryRow(ctx, `
		SELECT account_state, (SELECT count(*) FROM account_deletion_requests WHERE subject_id=$1)
		FROM users WHERE id=$1
	`, targetID).Scan(&targetState, &targetMarkers); err != nil {
		t.Fatal(err)
	}
	if targetState != user.AccountActive || targetMarkers != 0 {
		t.Fatalf("rejected requester changed target state=%s markers=%d", targetState, targetMarkers)
	}

	if err := users.AnonymizeAccount(ctx, subjectID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT requested_by FROM account_deletion_requests WHERE id=$1`, request.ID).Scan(&request.RequestedBy); err != nil {
		t.Fatal(err)
	}
	if request.RequestedBy != nil {
		t.Fatalf("anonymization retained self requester: %v", request.RequestedBy)
	}
	if err := competitors.Reinstate(ctx, existing.ID); !errors.Is(err, competitor.ErrConflict) {
		t.Fatalf("detached competitor reinstate error = %v", err)
	}
	var userID *uuid.UUID
	var withdrawnAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT user_id, withdrawn_at FROM competitors WHERE id=$1`, existing.ID).Scan(&userID, &withdrawnAt); err != nil {
		t.Fatal(err)
	}
	if userID != nil || withdrawnAt == nil {
		t.Fatalf("detached competitor was revived: user=%v withdrawn=%v", userID, withdrawnAt)
	}
}

func TestInFlightTicketWriterCommitsBeforeDeletionAndIsThenDetached(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	users := user.NewPostgresStore(pool)
	subjectID := uuid.New()
	if _, _, err := user.NewService(users).Ensure(ctx, subjectID, user.Profile{Email: "in-flight-ticket@example.test"}); err != nil {
		t.Fatal(err)
	}
	eventRow, err := event.NewPostgresStore(pool).Create(ctx, event.Event{
		Name: "Subject-lock race", Location: "YTÜ", OwnerTeam: "WEBLAB",
	})
	if err != nil {
		t.Fatal(err)
	}

	// The trigger takes the subject advisory lock before reading the marker.
	// Holding the marker table here freezes it at that boundary so we can prove
	// a later deletion request waits, then observes and detaches this ticket.
	blocker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(ctx)
	if _, err := blocker.Exec(ctx, `LOCK TABLE account_deletion_requests IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}

	createdID := uuid.New()
	ticketDone := make(chan error, 1)
	go func() {
		_, err := ticket.NewPostgresStore(pool).Create(ctx, ticket.Ticket{
			ID: createdID, EventID: eventRow.ID, TicketType: ticket.Registered, OwnerID: &subjectID,
		})
		ticketDone <- err
	}()
	testpostgres.WaitForBlockedQuery(t, pool, "INSERT INTO tickets")

	deletionDone := make(chan error, 1)
	go func() {
		if _, err := users.RequestDeletion(ctx, subjectID, nil); err != nil {
			deletionDone <- err
			return
		}
		deletionDone <- users.AnonymizeAccount(ctx, subjectID, time.Now().UTC())
	}()
	testpostgres.WaitForBlockedQuery(t, pool, "pg_advisory_xact_lock")

	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-ticketDone; err != nil {
		t.Fatal(err)
	}
	if err := <-deletionDone; err != nil {
		t.Fatal(err)
	}

	var ownerID *uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT owner_id FROM tickets WHERE id=$1`, createdID).Scan(&ownerID); err != nil {
		t.Fatal(err)
	}
	if ownerID != nil {
		t.Fatalf("in-flight ticket retained erased owner %s", *ownerID)
	}
}

func TestDirectSQLDeletionMarkerWaitsForGuardedSubjectWriter(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	users := user.NewPostgresStore(pool)
	subjectID := uuid.New()
	if _, _, err := user.NewService(users).Ensure(ctx, subjectID, user.Profile{Email: "direct-marker-race@example.test"}); err != nil {
		t.Fatal(err)
	}
	eventRow, err := event.NewPostgresStore(pool).Create(ctx, event.Event{
		Name: "Direct marker race", Location: "YTÜ", OwnerTeam: "WEBLAB",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Leave a real subject-link insert uncommitted after its row trigger has
	// acquired the per-subject advisory lock.
	writer, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback(ctx)
	ticketID := uuid.New()
	if _, err := writer.Exec(ctx, `
		INSERT INTO tickets (id,event_id,ticket_type,owner_id)
		VALUES ($1,$2,'REGISTERED',$3)
	`, ticketID, eventRow.ID, subjectID); err != nil {
		t.Fatal(err)
	}

	requestID := uuid.New()
	markerDone := make(chan error, 1)
	go func() {
		tx, err := pool.Begin(ctx)
		if err != nil {
			markerDone <- err
			return
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, `
			INSERT INTO account_deletion_requests (id,subject_id) VALUES ($1,$2)
		`, requestID, subjectID); err != nil {
			markerDone <- err
			return
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO account_deletion_outbox (id,request_id,subject_id) VALUES ($1,$2,$3)
		`, uuid.New(), requestID, subjectID); err != nil {
			markerDone <- err
			return
		}
		if _, err := tx.Exec(ctx, `
			UPDATE users SET account_state='deletion_pending',deletion_requested_at=now(),updated_at=now()
			WHERE id=$1
		`, subjectID); err != nil {
			markerDone <- err
			return
		}
		markerDone <- tx.Commit(ctx)
	}()
	testpostgres.WaitForBlockedQuery(t, pool, "INSERT INTO account_deletion_requests")

	if err := writer.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-markerDone; err != nil {
		t.Fatal(err)
	}
	if err := users.AnonymizeAccount(ctx, subjectID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	var ownerID *uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT owner_id FROM tickets WHERE id=$1`, ticketID).Scan(&ownerID); err != nil {
		t.Fatal(err)
	}
	if ownerID != nil {
		t.Fatalf("direct marker race retained ticket owner %s", *ownerID)
	}
}

func TestActorReferenceGuardsAreInstalledAndSerializeWithDeletion(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	expectedTriggers := []string{
		"events_require_active_archiver",
		"event_days_require_active_archiver",
		"sessions_require_active_archiver",
		"seasons_require_active_archiver",
		"media_require_active_deleter",
		"urls_require_active_disabler",
		"certificate_templates_require_active_creator",
		"certificate_template_versions_require_active_publisher",
		"certificate_template_bindings_require_active_updater",
		"certificate_event_state_require_active_finalizer",
		"certificate_batches_require_active_requester",
		"account_deletion_requests_require_active_subject",
		"account_deletion_requests_require_active_requester",
	}
	var installed int
	if err := pool.QueryRow(ctx, `
		SELECT count(DISTINCT trigger_name)
		FROM information_schema.triggers
		WHERE trigger_schema='public' AND trigger_name = ANY($1)
	`, expectedTriggers).Scan(&installed); err != nil {
		t.Fatal(err)
	}
	if installed != len(expectedTriggers) {
		t.Fatalf("actor reference triggers = %d, want %d", installed, len(expectedTriggers))
	}
	var competitorTrigger string
	if err := pool.QueryRow(ctx, `
		SELECT pg_get_triggerdef(oid)
		FROM pg_trigger
		WHERE tgrelid='competitors'::regclass
		  AND tgname='competitors_require_active_subject'
		  AND NOT tgisinternal
	`).Scan(&competitorTrigger); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(competitorTrigger, "UPDATE OF user_id, withdrawn_at, withdrawn_by") ||
		!strings.Contains(competitorTrigger, "'user_id', 'withdrawn_by'") {
		t.Fatalf("competitor guard does not acquire both identities together: %s", competitorTrigger)
	}

	users := user.NewPostgresStore(pool)
	actorID := uuid.New()
	if _, _, err := user.NewService(users).Ensure(ctx, actorID, user.Profile{Email: "archive-actor@example.test"}); err != nil {
		t.Fatal(err)
	}
	events := event.NewPostgresStore(pool)
	inFlight, err := events.Create(ctx, event.Event{Name: "In-flight archive", Location: "YTÜ", OwnerTeam: "WEBLAB"})
	if err != nil {
		t.Fatal(err)
	}
	blocked, err := events.Create(ctx, event.Event{Name: "Blocked archive", Location: "YTÜ", OwnerTeam: "WEBLAB"})
	if err != nil {
		t.Fatal(err)
	}

	blocker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(ctx)
	if _, err := blocker.Exec(ctx, `LOCK TABLE account_deletion_requests IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	archiveDone := make(chan error, 1)
	go func() { archiveDone <- events.Archive(ctx, inFlight.ID, &actorID) }()
	testpostgres.WaitForBlockedQuery(t, pool, "UPDATE events")

	deletionDone := make(chan error, 1)
	go func() {
		if _, err := users.RequestDeletion(ctx, actorID, nil); err != nil {
			deletionDone <- err
			return
		}
		deletionDone <- users.AnonymizeAccount(ctx, actorID, time.Now().UTC())
	}()
	testpostgres.WaitForBlockedQuery(t, pool, "pg_advisory_xact_lock")
	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-archiveDone; err != nil {
		t.Fatal(err)
	}
	if err := <-deletionDone; err != nil {
		t.Fatal(err)
	}

	var archivedAt *time.Time
	var archivedBy *uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT archived_at, archived_by FROM events WHERE id=$1`, inFlight.ID).Scan(&archivedAt, &archivedBy); err != nil {
		t.Fatal(err)
	}
	if archivedAt == nil || archivedBy != nil {
		t.Fatalf("in-flight archive attribution was not scrubbed: at=%v by=%v", archivedAt, archivedBy)
	}
	if err := events.Archive(ctx, blocked.ID, &actorID); !errors.Is(err, event.ErrForbidden) {
		t.Fatalf("archive with deleted actor error = %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT archived_at, archived_by FROM events WHERE id=$1`, blocked.ID).Scan(&archivedAt, &archivedBy); err != nil {
		t.Fatal(err)
	}
	if archivedAt != nil || archivedBy != nil {
		t.Fatalf("rejected archive mutated row: at=%v by=%v", archivedAt, archivedBy)
	}
}

func TestCompetitorCrossWithdrawAcquiresSubjectAndActorLocksInOneOrder(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	users := user.NewPostgresStore(pool)
	lowID := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	highID := uuid.MustParse("ffffffff-ffff-ffff-ffff-fffffffffff1")
	if _, _, err := user.NewService(users).Ensure(ctx, lowID, user.Profile{Email: "low-lock@example.test"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := user.NewService(users).Ensure(ctx, highID, user.Profile{Email: "high-lock@example.test"}); err != nil {
		t.Fatal(err)
	}
	eventRow, err := event.NewPostgresStore(pool).Create(ctx, event.Event{
		Name: "Cross withdraw", Location: "YTÜ", OwnerTeam: "WEBLAB",
	})
	if err != nil {
		t.Fatal(err)
	}
	store := competitor.NewPostgresStore(pool)
	lowCompetitor, err := store.Create(ctx, competitor.Competitor{UserID: lowID, EventID: eventRow.ID})
	if err != nil {
		t.Fatal(err)
	}
	highCompetitor, err := store.Create(ctx, competitor.Competitor{UserID: highID, EventID: eventRow.ID})
	if err != nil {
		t.Fatal(err)
	}

	// This trigger widens the boundary between the former subject-only and
	// withdrawer-only triggers. With the old two-trigger design both updates
	// hold their own subject lock here and deterministically deadlock next.
	if _, err := pool.Exec(ctx, `
		CREATE FUNCTION public.test_competitor_guard_pause() RETURNS trigger
		LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_sleep(0.2); RETURN NEW; END $$;
		CREATE TRIGGER competitors_require_active_subject_pause
		BEFORE UPDATE OF withdrawn_by ON competitors
		FOR EACH ROW EXECUTE FUNCTION public.test_competitor_guard_pause();
	`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `
			DROP TRIGGER IF EXISTS competitors_require_active_subject_pause ON competitors;
			DROP FUNCTION IF EXISTS public.test_competitor_guard_pause();
		`)
	})

	start := make(chan struct{})
	done := make(chan error, 2)
	go func() {
		<-start
		done <- store.Withdraw(ctx, lowCompetitor.ID, &highID)
	}()
	go func() {
		<-start
		done <- store.Withdraw(ctx, highCompetitor.ID, &lowID)
	}()
	close(start)
	for range 2 {
		if err := <-done; err != nil {
			t.Fatalf("cross-withdraw failed: %v", err)
		}
	}
}
