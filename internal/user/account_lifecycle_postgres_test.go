package user_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/certificate"
	"github.com/skylab-kulubu/core-backend/internal/competitor"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

func TestPostgresAccountAnonymizationDetachesIdentityAndPreservesHistory(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := user.NewPostgresStore(pool)
	service := user.NewService(store)
	subjectID := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	profileMediaID := uuid.New()

	created, _, err := service.Ensure(ctx, subjectID, user.Profile{
		Email: "ada@example.com", FirstName: "Ada", LastName: "Lovelace",
		Username: "ada", SchoolEmail: "ada@std.yildiz.edu.tr", SkyNumber: "SKY-0000042",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO media (id, file_name, file_type, file_url, file_size, uploaded_by, kind)
		VALUES ($1, 'profile.png', 'image/png', 'images/profile.png', 10, $2, 'IMAGE')
	`, profileMediaID, subjectID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE users SET
			student_card_uid = 'card-uid', linkedin = 'https://linkedin.example/ada',
			university = 'YTÜ', faculty = 'Elektrik-Elektronik', department = 'Bilgisayar',
			phone = '+905551112233', profile_picture_id = $2, profile_picture_url = 'images/profile.png'
		WHERE id = $1
	`, subjectID, profileMediaID); err != nil {
		t.Fatal(err)
	}

	eventID := uuid.New()
	eventDayID := uuid.New()
	sessionID := uuid.New()
	ticketID := uuid.New()
	checkInID := uuid.New()
	competitorID := uuid.New()
	certificateID := uuid.New()
	urlID := uuid.New()
	hitID := uuid.New()
	certificatePDF := []byte("%PDF-retained-certificate")
	if _, err := pool.Exec(ctx, `INSERT INTO events (id, name, location, owner_team) VALUES ($1, 'Tarih', 'YTÜ', 'WEBLAB')`, eventID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO event_days (id, event_id, name) VALUES ($1, $2, 'Gün 1')`, eventDayID, eventID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sessions (id, event_day_id, title, session_type) VALUES ($1, $2, 'Oturum', 'PRESENTATION')`, sessionID, eventDayID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO tickets (id, event_id, ticket_type, owner_id) VALUES ($1, $2, 'REGISTERED', $3)`, ticketID, eventID, subjectID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO ticket_checkins (id, ticket_id, event_day_id, session_id) VALUES ($1, $2, $3, $4)`, checkInID, ticketID, eventDayID, sessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO competitors (id, user_id, event_id) VALUES ($1, $2, $3)`, competitorID, subjectID, eventID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO certificates (
			id, event_id, ticket_id, owner_id, serial, recipient_name, recipient_email,
			event_name, owner_team, verify_url, pdf
		) VALUES ($1, $2, $3, $4, 'SERIAL-1', 'Ada Lovelace', 'ada@example.com', 'Tarih', 'WEBLAB', 'https://example.test/c/1', $5)
	`, certificateID, eventID, ticketID, subjectID, certificatePDF); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO urls (id, alias, url, created_by) VALUES ($1, 'ada-link', 'https://example.test', $2)`, urlID, subjectID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO url_hits (id, url_id, alias, at, ip, user_agent, referer, user_id)
		VALUES ($1, $2, 'ada-link', now(), '192.0.2.42', 'Ada Browser', 'https://private.example/ada', $3)
	`, hitID, urlID, subjectID); err != nil {
		t.Fatal(err)
	}

	first, err := store.RequestDeletion(ctx, subjectID, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.RequestDeletion(ctx, subjectID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Fatalf("request IDs differ: %s != %s", first.ID, second.ID)
	}
	var requestCount, outboxCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM account_deletion_requests WHERE subject_id = $1`, subjectID).Scan(&requestCount); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM account_deletion_outbox WHERE subject_id = $1`, subjectID).Scan(&outboxCount); err != nil {
		t.Fatal(err)
	}
	if requestCount != 1 || outboxCount != 1 {
		t.Fatalf("requests=%d outbox=%d", requestCount, outboxCount)
	}
	var outboxEvent string
	if err := pool.QueryRow(ctx, `SELECT event_type FROM account_deletion_outbox WHERE request_id = $1`, first.ID).Scan(&outboxEvent); err != nil {
		t.Fatal(err)
	}
	if outboxEvent != "account.deletion_requested" {
		t.Fatalf("outbox event = %q", outboxEvent)
	}
	if _, _, err := service.Ensure(ctx, subjectID, user.Profile{Email: "old-token@example.com"}); !errors.Is(err, user.ErrAccountBlocked) {
		t.Fatalf("old token Ensure error = %v", err)
	}
	if allowed, err := store.CanAttribute(ctx, subjectID); err != nil || allowed {
		t.Fatalf("deletion-pending attribution allowed=%v err=%v", allowed, err)
	}

	anonymizedAt := time.Date(2026, 9, 20, 1, 2, 3, 0, time.UTC)
	if err := store.AnonymizeAccount(ctx, subjectID, anonymizedAt); err != nil {
		t.Fatal(err)
	}
	if err := store.AnonymizeAccount(ctx, subjectID, anonymizedAt.Add(time.Hour)); err != nil {
		t.Fatalf("anonymization retry: %v", err)
	}
	tombstone, err := store.Get(ctx, subjectID)
	if err != nil {
		t.Fatal(err)
	}
	if tombstone.AccountState != user.AccountAnonymized || tombstone.AnonymizedAt == nil || !tombstone.AnonymizedAt.Equal(anonymizedAt) {
		t.Fatalf("tombstone state: %+v", tombstone)
	}
	if tombstone.Email != "" || tombstone.FirstName != "" || tombstone.LastName != "" ||
		tombstone.Username != "" || tombstone.SchoolEmail != "" || tombstone.SkyNumber != "" ||
		tombstone.StudentCardUID != "" || tombstone.Linkedin != "" || tombstone.University != "" ||
		tombstone.Faculty != "" || tombstone.Department != "" || tombstone.Phone != "" ||
		tombstone.ProfilePictureID != nil || tombstone.ProfilePictureURL != "" {
		t.Fatalf("tombstone retained PII: %+v (created=%+v)", tombstone, created)
	}

	for _, check := range []struct {
		name  string
		query string
		id    uuid.UUID
	}{
		{name: "ticket", query: `SELECT count(*) FROM tickets WHERE id = $1 AND owner_id IS NULL`, id: ticketID},
		{name: "check-in", query: `SELECT count(*) FROM ticket_checkins WHERE id = $1 AND ticket_id IS NOT NULL`, id: checkInID},
		{name: "competitor", query: `SELECT count(*) FROM competitors WHERE id = $1 AND user_id IS NULL`, id: competitorID},
		{name: "media", query: `SELECT count(*) FROM media WHERE id = $1 AND uploaded_by IS NULL`, id: profileMediaID},
		{name: "certificate", query: `SELECT count(*) FROM certificates WHERE id = $1 AND owner_id IS NULL AND recipient_email = '' AND recipient_name = 'Ada Lovelace' AND serial = 'SERIAL-1'`, id: certificateID},
	} {
		var count int
		if err := pool.QueryRow(ctx, check.query, check.id).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("%s history not preserved and detached", check.name)
		}
	}
	var hitUserID *uuid.UUID
	var hitIP, hitUserAgent, hitReferer string
	if err := pool.QueryRow(ctx, `SELECT user_id, ip, user_agent, referer FROM url_hits WHERE id = $1`, hitID).Scan(&hitUserID, &hitIP, &hitUserAgent, &hitReferer); err != nil {
		t.Fatal(err)
	}
	if hitUserID != nil || hitIP != "" || hitUserAgent != "" || hitReferer != "" {
		t.Fatalf("URL hit retained subject PII: user=%v ip=%q agent=%q referer=%q", hitUserID, hitIP, hitUserAgent, hitReferer)
	}
	if got, err := competitor.NewPostgresStore(pool).GetIncludingWithdrawn(ctx, competitorID); err != nil || got.UserID != uuid.Nil || got.WithdrawnAt == nil {
		t.Fatalf("detached competitor read = %+v, err=%v", got, err)
	}
	if got, err := media.NewPostgresStore(pool).GetIncludingDeleted(ctx, profileMediaID); err != nil || got.UploadedBy != uuid.Nil {
		t.Fatalf("detached media read = %+v, err=%v", got, err)
	}
	if got, err := ticket.NewPostgresStore(pool).Get(ctx, ticketID); err != nil || got.OwnerID != nil || len(got.CheckIns) != 1 || got.CheckIns[0].ID != checkInID {
		t.Fatalf("detached ticket read = %+v, err=%v", got, err)
	}
	certificateStore := certificate.NewPostgresStore(pool)
	if got, _, err := certificateStore.GetBySerial(ctx, "SERIAL-1"); err != nil || got.OwnerID != nil || got.RecipientEmail != "" || got.RecipientName != "Ada Lovelace" {
		t.Fatalf("verifiable certificate read = %+v, err=%v", got, err)
	}
	certificateService := certificate.NewService(certificateStore, nil, nil, nil, nil, nil, nil, "https://api.example.test")
	if public, err := certificateService.Verify(ctx, "SERIAL-1"); err != nil || public.Serial != "SERIAL-1" || public.RecipientName != "Ada Lovelace" || public.Status != "valid" {
		t.Fatalf("public verification = %+v, err=%v", public, err)
	}
	if pdf, err := certificateService.PDF(ctx, "SERIAL-1"); err != nil || string(pdf) != string(certificatePDF) {
		t.Fatalf("certificate PDF = %q, err=%v", pdf, err)
	}

	if _, err := pool.Exec(ctx, `UPDATE account_deletion_requests SET status = 'completed', completed_at = now() WHERE id = $1`, first.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.HardPurgeAccount(ctx, subjectID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, subjectID); !errors.Is(err, user.ErrNotFound) {
		t.Fatalf("hard-purged tombstone Get error = %v", err)
	}
	if _, _, err := service.Ensure(ctx, subjectID, user.Profile{Email: "old-token@example.com"}); !errors.Is(err, user.ErrAccountBlocked) {
		t.Fatalf("hard purge allowed resurrection: %v", err)
	}
	if allowed, err := store.CanAttribute(ctx, subjectID); err != nil || allowed {
		t.Fatalf("hard-purged attribution allowed=%v err=%v", allowed, err)
	}
	if public, err := certificateService.Verify(ctx, "SERIAL-1"); err != nil || public.Serial != "SERIAL-1" || public.RecipientName != "Ada Lovelace" || public.Status != "valid" {
		t.Fatalf("public verification after hard purge = %+v, err=%v", public, err)
	}
	if pdf, err := certificateService.PDF(ctx, "SERIAL-1"); err != nil || string(pdf) != string(certificatePDF) {
		t.Fatalf("certificate PDF after hard purge = %q, err=%v", pdf, err)
	}
}

func TestConcurrentAnonymizersAcquireReferenceTableLocksWithoutUpgradeDeadlock(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := user.NewPostgresStore(pool)
	firstID := uuid.New()
	secondID := uuid.New()
	for id, email := range map[uuid.UUID]string{firstID: "first-anonymizer@example.test", secondID: "second-anonymizer@example.test"} {
		if _, _, err := user.NewService(store).Ensure(ctx, id, user.Profile{Email: email}); err != nil {
			t.Fatal(err)
		}
	}
	eventID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO events (id,name,location,owner_team) VALUES ($1,'Anonymize race','YTÜ','WEBLAB')`, eventID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO competitors (id,user_id,event_id) VALUES ($1,$2,$3),($4,$5,$3)
	`, uuid.New(), firstID, eventID, uuid.New(), secondID); err != nil {
		t.Fatal(err)
	}
	for _, id := range []uuid.UUID{firstID, secondID} {
		if _, err := store.RequestDeletion(ctx, id, nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `
		CREATE FUNCTION public.test_anonymize_pause() RETURNS trigger
		LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_sleep(0.2); RETURN NEW; END $$;
		CREATE TRIGGER test_anonymize_pause
		BEFORE UPDATE OF user_id ON competitors
		FOR EACH ROW EXECUTE FUNCTION public.test_anonymize_pause();
	`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `
			DROP TRIGGER IF EXISTS test_anonymize_pause ON competitors;
			DROP FUNCTION IF EXISTS public.test_anonymize_pause();
		`)
	})

	start := make(chan struct{})
	done := make(chan error, 2)
	for _, id := range []uuid.UUID{firstID, secondID} {
		id := id
		go func() {
			<-start
			done <- store.AnonymizeAccount(ctx, id, time.Now().UTC())
		}()
	}
	close(start)
	for range 2 {
		if err := <-done; err != nil {
			t.Fatalf("concurrent anonymization failed: %v", err)
		}
	}
}
