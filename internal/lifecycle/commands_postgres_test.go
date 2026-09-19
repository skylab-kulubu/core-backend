package lifecycle_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/certificate"
	"github.com/skylab-kulubu/core-backend/internal/competitor"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/season"
	"github.com/skylab-kulubu/core-backend/internal/shorturl"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

func TestPostgresEventArchivePreservesDependentRecords(t *testing.T) {
	pool := lifecyclePool(t)
	ctx := context.Background()

	actorID := ensureLifecycleUser(t, pool, ctx, "event-archive-operator@example.com")
	attendeeID := ensureLifecycleUser(t, pool, ctx, "event-archive-attendee@example.com")
	eventStore := event.NewPostgresStore(pool)
	ticketStore := ticket.NewPostgresStore(pool)
	certificateStore := certificate.NewPostgresStore(pool)

	createdEvent, err := eventStore.Create(ctx, event.Event{
		Name:      "Durable event",
		Location:  "YTÜ",
		OwnerTeam: "WEBLAB",
		Active:    true,
	})
	if err != nil {
		t.Fatal(err)
	}
	day, err := eventStore.CreateDay(ctx, event.Day{EventID: createdEvent.ID, Name: "Day 1"})
	if err != nil {
		t.Fatal(err)
	}
	session, err := eventStore.CreateSession(ctx, event.Session{
		EventDayID:  day.ID,
		Title:       "Opening",
		SessionType: "PRESENTATION",
	})
	if err != nil {
		t.Fatal(err)
	}
	issuedTicket, err := ticketStore.Create(ctx, ticket.Ticket{
		EventID:    createdEvent.ID,
		TicketType: ticket.Registered,
		OwnerID:    &attendeeID,
	})
	if err != nil {
		t.Fatal(err)
	}
	checkIn, err := ticketStore.AddCheckIn(ctx, ticket.CheckIn{
		TicketID:   issuedTicket.ID,
		EventDayID: day.ID,
		SessionID:  session.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	const serial = "event-archive-preserves-certificate"
	const pdfBody = "durable-certificate-pdf"
	issuedCertificate, err := certificateStore.Create(ctx, certificate.Certificate{
		EventID:        createdEvent.ID,
		TicketID:       issuedTicket.ID,
		OwnerID:        &attendeeID,
		Serial:         serial,
		RecipientName:  "Durable Attendee",
		RecipientEmail: "event-archive-attendee@example.com",
		EventName:      createdEvent.Name,
		OwnerTeam:      createdEvent.OwnerTeam,
		VerifyURL:      "https://skyl.app/c/" + serial,
	}, []byte(pdfBody))
	if err != nil {
		t.Fatal(err)
	}

	if err := eventStore.Archive(ctx, createdEvent.ID, &actorID); err != nil {
		t.Fatal(err)
	}
	archivedOnce, err := eventStore.GetIncludingArchived(ctx, createdEvent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if archivedOnce.ArchivedAt == nil || archivedOnce.ArchivedBy == nil || *archivedOnce.ArchivedBy != actorID {
		t.Fatalf("first archive state = %+v", archivedOnce)
	}
	if err := eventStore.Archive(ctx, createdEvent.ID, &actorID); err != nil {
		t.Fatalf("second archive: %v", err)
	}
	archivedTwice, err := eventStore.GetIncludingArchived(ctx, createdEvent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if archivedTwice.ArchivedAt == nil || !archivedTwice.ArchivedAt.Equal(*archivedOnce.ArchivedAt) {
		t.Fatalf("second archive changed archived_at: first=%v second=%v", archivedOnce.ArchivedAt, archivedTwice.ArchivedAt)
	}
	if _, err := eventStore.Get(ctx, createdEvent.ID); !errors.Is(err, event.ErrNotFound) {
		t.Fatalf("archived event Get error = %v, want ErrNotFound", err)
	}
	if _, err := eventStore.GetDay(ctx, day.ID); !errors.Is(err, event.ErrNotFound) {
		t.Fatalf("day under archived event Get error = %v, want ErrNotFound", err)
	}
	if _, err := eventStore.GetSession(ctx, session.ID); !errors.Is(err, event.ErrNotFound) {
		t.Fatalf("session under archived event Get error = %v, want ErrNotFound", err)
	}

	assertLifecycleRowCount(t, pool, ctx, "event", `SELECT count(*) FROM events WHERE id = $1`, createdEvent.ID, 1)
	assertLifecycleRowCount(t, pool, ctx, "event day", `SELECT count(*) FROM event_days WHERE id = $1 AND event_id = $2`, day.ID, 1, createdEvent.ID)
	assertLifecycleRowCount(t, pool, ctx, "session", `SELECT count(*) FROM sessions WHERE id = $1 AND event_day_id = $2`, session.ID, 1, day.ID)
	assertLifecycleRowCount(t, pool, ctx, "ticket", `SELECT count(*) FROM tickets WHERE id = $1 AND event_id = $2`, issuedTicket.ID, 1, createdEvent.ID)
	assertLifecycleRowCount(t, pool, ctx, "check-in", `SELECT count(*) FROM ticket_checkins WHERE id = $1 AND ticket_id = $2`, checkIn.ID, 1, issuedTicket.ID)
	assertLifecycleRowCount(t, pool, ctx, "certificate", `SELECT count(*) FROM certificates WHERE id = $1 AND event_id = $2`, issuedCertificate.ID, 1, createdEvent.ID)

	preservedTicket, err := ticketStore.Get(ctx, issuedTicket.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(preservedTicket.CheckIns) != 1 || preservedTicket.CheckIns[0].ID != checkIn.ID {
		t.Fatalf("preserved ticket check-ins = %+v", preservedTicket.CheckIns)
	}
	preservedCertificate, pdf, err := certificateStore.GetBySerial(ctx, serial)
	if err != nil {
		t.Fatal(err)
	}
	if preservedCertificate.ID != issuedCertificate.ID || string(pdf) != pdfBody {
		t.Fatalf("preserved certificate = %+v, pdf = %q", preservedCertificate, pdf)
	}

	if err := eventStore.Restore(ctx, createdEvent.ID); err != nil {
		t.Fatal(err)
	}
	if err := eventStore.Restore(ctx, createdEvent.ID); err != nil {
		t.Fatalf("second restore: %v", err)
	}
	restored, err := eventStore.Get(ctx, createdEvent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if restored.ArchivedAt != nil || restored.ArchivedBy != nil {
		t.Fatalf("restored event state = %+v", restored)
	}
	if _, err := eventStore.GetDay(ctx, day.ID); err != nil {
		t.Fatalf("restored event day: %v", err)
	}
	if _, err := eventStore.GetSession(ctx, session.ID); err != nil {
		t.Fatalf("restored event session: %v", err)
	}
}

func TestPostgresShortURLDisablePreservesHitHistory(t *testing.T) {
	pool := lifecyclePool(t)
	ctx := context.Background()

	actorID := ensureLifecycleUser(t, pool, ctx, "url-disable-operator@example.com")
	store := shorturl.NewPostgresStore(pool)
	created, err := store.Create(ctx, shorturl.URL{
		Alias:     "durable-link",
		URL:       "https://example.com/durable",
		CreatedBy: &actorID,
	})
	if err != nil {
		t.Fatal(err)
	}
	recordedAt := time.Now().UTC().Add(-time.Minute)
	clicked, err := store.RecordHit(ctx, created.ID, shorturl.Hit{
		CreatedAt: recordedAt,
		IP:        "192.0.2.10",
		UserAgent: "lifecycle-contract-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if clicked.ClickCount != 1 {
		t.Fatalf("click count = %d, want 1", clicked.ClickCount)
	}

	if err := store.Disable(ctx, created.ID, &actorID); err != nil {
		t.Fatal(err)
	}
	disabledOnce, err := store.GetIncludingDisabled(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if disabledOnce.DisabledAt == nil || disabledOnce.DisabledBy == nil || *disabledOnce.DisabledBy != actorID {
		t.Fatalf("first disable state = %+v", disabledOnce)
	}
	if err := store.Disable(ctx, created.ID, &actorID); err != nil {
		t.Fatalf("second disable: %v", err)
	}
	disabledTwice, err := store.GetIncludingDisabled(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if disabledTwice.DisabledAt == nil || !disabledTwice.DisabledAt.Equal(*disabledOnce.DisabledAt) {
		t.Fatalf("second disable changed disabled_at: first=%v second=%v", disabledOnce.DisabledAt, disabledTwice.DisabledAt)
	}
	if _, err := store.GetByAlias(ctx, created.Alias); !errors.Is(err, shorturl.ErrNotFound) {
		t.Fatalf("disabled alias error = %v, want ErrNotFound", err)
	}
	if _, err := store.ListHits(ctx, created.ID, time.Time{}); !errors.Is(err, shorturl.ErrNotFound) {
		t.Fatalf("disabled hit history error = %v, want ErrNotFound", err)
	}
	assertLifecycleRowCount(t, pool, ctx, "short URL", `SELECT count(*) FROM urls WHERE id = $1`, created.ID, 1)
	assertLifecycleRowCount(t, pool, ctx, "URL hit", `SELECT count(*) FROM url_hits WHERE url_id = $1`, created.ID, 1)

	if err := store.Restore(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.Restore(ctx, created.ID); err != nil {
		t.Fatalf("second restore: %v", err)
	}
	restored, err := store.GetByAlias(ctx, created.Alias)
	if err != nil {
		t.Fatal(err)
	}
	if restored.DisabledAt != nil || restored.DisabledBy != nil || restored.ClickCount != 1 {
		t.Fatalf("restored URL = %+v", restored)
	}
	hits, err := store.ListHits(ctx, created.ID, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].URLID != created.ID || hits[0].UserAgent != "lifecycle-contract-test" {
		t.Fatalf("restored hit history = %+v", hits)
	}
}

func TestPostgresLifecycleCommandsAreIdempotent(t *testing.T) {
	pool := lifecyclePool(t)
	ctx := context.Background()

	actorID := ensureLifecycleUser(t, pool, ctx, "transition-operator@example.com")
	participantID := ensureLifecycleUser(t, pool, ctx, "transition-participant@example.com")
	eventStore := event.NewPostgresStore(pool)
	createdEvent, err := eventStore.Create(ctx, event.Event{
		Name: "Lifecycle commands", Location: "YTÜ", OwnerTeam: "WEBLAB", Active: true, Ranked: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	day, err := eventStore.CreateDay(ctx, event.Day{EventID: createdEvent.ID, Name: "Day"})
	if err != nil {
		t.Fatal(err)
	}
	session, err := eventStore.CreateSession(ctx, event.Session{
		EventDayID: day.ID, Title: "Session", SessionType: "PRESENTATION",
	})
	if err != nil {
		t.Fatal(err)
	}
	seasonStore := season.NewPostgresStore(pool)
	createdSeason, err := seasonStore.Create(ctx, season.Season{Name: "2026-2027", Active: true})
	if err != nil {
		t.Fatal(err)
	}
	competitorStore := competitor.NewPostgresStore(pool)
	createdCompetitor, err := competitorStore.Create(ctx, competitor.Competitor{
		UserID: participantID, EventID: createdEvent.ID,
	})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		archive func() error
		restore func() error
		state   func() (*time.Time, *uuid.UUID, error)
	}{
		{
			name:    "event day",
			archive: func() error { return eventStore.ArchiveDay(ctx, day.ID, &actorID) },
			restore: func() error { return eventStore.RestoreDay(ctx, day.ID) },
			state: func() (*time.Time, *uuid.UUID, error) {
				got, err := eventStore.GetDayIncludingArchived(ctx, day.ID)
				return got.ArchivedAt, got.ArchivedBy, err
			},
		},
		{
			name:    "session",
			archive: func() error { return eventStore.ArchiveSession(ctx, session.ID, &actorID) },
			restore: func() error { return eventStore.RestoreSession(ctx, session.ID) },
			state: func() (*time.Time, *uuid.UUID, error) {
				got, err := eventStore.GetSessionIncludingArchived(ctx, session.ID)
				return got.ArchivedAt, got.ArchivedBy, err
			},
		},
		{
			name:    "season",
			archive: func() error { return seasonStore.Archive(ctx, createdSeason.ID, &actorID) },
			restore: func() error { return seasonStore.Restore(ctx, createdSeason.ID) },
			state: func() (*time.Time, *uuid.UUID, error) {
				got, err := seasonStore.GetIncludingArchived(ctx, createdSeason.ID)
				return got.ArchivedAt, got.ArchivedBy, err
			},
		},
		{
			name:    "competitor",
			archive: func() error { return competitorStore.Withdraw(ctx, createdCompetitor.ID, &actorID) },
			restore: func() error { return competitorStore.Reinstate(ctx, createdCompetitor.ID) },
			state: func() (*time.Time, *uuid.UUID, error) {
				got, err := competitorStore.GetIncludingWithdrawn(ctx, createdCompetitor.ID)
				return got.WithdrawnAt, got.WithdrawnBy, err
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.archive(); err != nil {
				t.Fatal(err)
			}
			firstAt, firstBy, err := tc.state()
			if err != nil {
				t.Fatal(err)
			}
			if firstAt == nil || firstBy == nil || *firstBy != actorID {
				t.Fatalf("first inactive state: at=%v by=%v", firstAt, firstBy)
			}
			if err := tc.archive(); err != nil {
				t.Fatalf("second archive: %v", err)
			}
			secondAt, secondBy, err := tc.state()
			if err != nil {
				t.Fatal(err)
			}
			if secondAt == nil || !secondAt.Equal(*firstAt) || secondBy == nil || *secondBy != actorID {
				t.Fatalf("second inactive state: firstAt=%v secondAt=%v secondBy=%v", firstAt, secondAt, secondBy)
			}
			if err := tc.restore(); err != nil {
				t.Fatal(err)
			}
			if err := tc.restore(); err != nil {
				t.Fatalf("second restore: %v", err)
			}
			restoredAt, restoredBy, err := tc.state()
			if err != nil {
				t.Fatal(err)
			}
			if restoredAt != nil || restoredBy != nil {
				t.Fatalf("restored state: at=%v by=%v", restoredAt, restoredBy)
			}
		})
	}
}

func ensureLifecycleUser(t *testing.T, pool *pgxpool.Pool, ctx context.Context, email string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, _, err := user.NewService(user.NewPostgresStore(pool)).Ensure(ctx, id, user.Profile{
		Email: email, FirstName: "Lifecycle", LastName: "Fixture", Username: id.String(),
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

func assertLifecycleRowCount(
	t *testing.T,
	pool *pgxpool.Pool,
	ctx context.Context,
	name string,
	query string,
	firstArg any,
	want int,
	additionalArgs ...any,
) {
	t.Helper()
	args := append([]any{firstArg}, additionalArgs...)
	var got int
	if err := pool.QueryRow(ctx, query, args...).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("%s row count = %d, want %d", name, got, want)
	}
}
