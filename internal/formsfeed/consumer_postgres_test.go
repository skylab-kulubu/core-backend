package formsfeed_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/events"
	"github.com/skylab-kulubu/core-backend/internal/formsfeed"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

// flakyForms fails its first read, as Forms down for a moment.
type flakyForms struct{ calls int }

func (f *flakyForms) Response(_ context.Context, formID, responseID uuid.UUID, _ ...string) (formsfeed.Response, error) {
	f.calls++
	if f.calls == 1 {
		return formsfeed.Response{}, errors.New("formsfeed: Forms answered 503")
	}
	return formsfeed.Response{FormID: formID, ResponseID: responseID, Status: formsfeed.StatusAccepted, HasGuest: true, Version: 1,
		Guest: &formsfeed.Guest{FirstName: "Ada", LastName: "Lovelace", Email: "ada@example.com"}}, nil
}

// The consumer's record, the version and the Ticket rule against one
// PostgreSQL: a read that fails is transient (not the DLQ) and records
// nothing; the event handed over again writes the guest's Ticket once; a
// third delivery is a duplicate.
func TestCoreFormsEventEndToEndOnPostgres(t *testing.T) {
	pool := migrated(t)
	ctx := context.Background()
	evStore := event.NewPostgresStore(pool)
	tickets := ticket.NewPostgresStore(pool)
	tickets.NotifyCreated(true)
	formID := uuid.New()
	start, end := time.Now().Add(24*time.Hour), time.Now().Add(26*time.Hour)
	ev, err := evStore.Create(ctx, event.Event{
		Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB", StartDate: &start, EndDate: &end,
		FormURL: "https://forms.yildizskylab.com/" + formID.String(),
	})
	if err != nil {
		t.Fatal(err)
	}
	svc := ticket.NewService(tickets, evStore, authz.NewAuthorizer(authz.DefaultPolicy()))
	forms := &flakyForms{}
	rec := &formsfeed.Recorder{
		Events: evStore, Forms: forms,
		Record: func(ctx context.Context, r ticket.FormResponse) (ticket.FormResponseResult, error) {
			return svc.RecordFormResponse(ctx, formsService, r)
		},
	}
	handle := formsfeed.Subscription(rec).Handle
	data, _ := json.Marshal(map[string]any{
		"formId": formID, "responseId": uuid.New(), "status": "accepted", "userId": nil, "hasGuest": true,
		"submittedAt": time.Now().UTC().Format(time.RFC3339), "hiddenFields": map[string]any{}, "version": 3,
	})
	e := events.Event{ID: uuid.NewString(), Source: formsfeed.Source, Type: formsfeed.TypeResponseCreated, Data: data, Queue: formsfeed.Queue}
	store := events.NewPostgres(pool)
	consume := func() (bool, error) {
		return store.Consume(ctx, formsfeed.Queue, e.Source, e.ID, func(tx pgx.Tx) error { return handle(ctx, tx, e) })
	}
	count := func(sql string, args ...any) int {
		var n int
		if err := pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	if _, err := consume(); err == nil || events.IsPermanent(err) || errors.Is(err, events.ErrUnrelated) {
		t.Fatalf("first delivery: %v, want a transient error", err)
	}
	if count(`SELECT count(*) FROM consumed_events`) != 0 || count(`SELECT count(*) FROM forms_record_versions`) != 0 ||
		count(`SELECT count(*) FROM tickets WHERE event_id = $1`, ev.ID) != 0 {
		t.Fatal("a failed delivery left a record, a version or a Ticket")
	}
	if dup, err := consume(); err != nil || dup {
		t.Fatalf("second delivery: %t %v", dup, err)
	}
	if count(`SELECT count(*) FROM tickets WHERE event_id = $1 AND guest_email = 'ada@example.com'`, ev.ID) != 1 ||
		count(`SELECT count(*) FROM consumed_events WHERE consumer = 'core.forms'`) != 1 {
		t.Fatal("the redelivery did not write the guest's Ticket once")
	}
	if dup, err := consume(); err != nil || !dup {
		t.Fatalf("third delivery: %t %v, want a duplicate", dup, err)
	}
	if n := count(`SELECT count(*) FROM tickets WHERE event_id = $1`, ev.ID); n != 1 || forms.calls != 2 {
		t.Fatalf("tickets %d, reads %d", n, forms.calls)
	}
}
