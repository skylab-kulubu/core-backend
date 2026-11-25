package event_test

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
)

// The other details (event.changed): told only when the editor says yes and
// their telling is on; a body without the answer tells none of them; the
// waiting row keeps the first change's digest of each, and a later time
// change joins it. Off, a change of details neither queues nor is recorded.
func TestOtherDetailsAreToldOnlyWhenTheEditorSaysYes(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := event.NewPostgresStore(pool)
	svc := event.NewService(store, authz.NewAuthorizer(authz.DefaultPolicy()))
	start := time.Now().Add(72 * time.Hour).UTC().Truncate(time.Second)
	published := func() event.Event {
		t.Helper()
		e, err := svc.Create(ctx, copyLeader, event.Event{Name: "Gece Kodu", Description: "İlk", Location: "D-Blok", OwnerTeam: "WEBLAB", Status: event.StatusPublished, StartDate: &start, Capacity: 40})
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	patch := func(id uuid.UUID, body string) {
		t.Helper()
		var b event.Patch
		if err := json.Unmarshal([]byte(body), &b); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.Patch(ctx, copyLeader, id, b); err != nil {
			t.Fatalf("patch %s: %v", body, err)
		}
	}
	row := func(id uuid.UUID) (int, []string, map[string]string) {
		t.Helper()
		var n int
		var told []string
		var digests map[string]string
		if err := pool.QueryRow(ctx, `SELECT count(*), (SELECT told_fields FROM event_update_notifications WHERE event_id = $1 LIMIT 1),
			COALESCE((SELECT before_details FROM event_update_notifications WHERE event_id = $1 LIMIT 1), '{}')
			FROM event_update_notifications WHERE event_id = $1`, id).Scan(&n, &told, &digests); err != nil {
			t.Fatal(err)
		}
		return n, told, digests
	}
	choices := func(id uuid.UUID) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM event_change_notice_choices WHERE event_id = $1`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	store.NotifyUpdated(true)

	off := published()
	patch(off.ID, `{"description": "Yeni", "notifyTicketHolders": true}`)
	if n, _, _ := row(off.ID); n != 0 || choices(off.ID) != 0 {
		t.Fatalf("details with their telling off: rows %d, choices %d", n, choices(off.ID))
	}

	store.NotifyChanged(true)
	silent := published()
	patch(silent.ID, `{"description": "Yeni"}`)
	if n, _, _ := row(silent.ID); n != 0 {
		t.Fatalf("a body without the answer queued %d rows", n)
	}
	no := published()
	patch(no.ID, `{"capacity": 60, "notifyTicketHolders": false}`)
	if n, _, _ := row(no.ID); n != 0 || choices(no.ID) != 1 {
		t.Fatalf("no: rows %d, choices %d", n, choices(no.ID))
	}

	yes := published()
	firstDigest := event.DetailDigests(yes)[event.FieldDescription]
	patch(yes.ID, `{"description": "İkinci", "capacity": 50, "notifyTicketHolders": true}`)
	patch(yes.ID, `{"description": "Üçüncü", "notifyTicketHolders": true}`)
	patch(yes.ID, `{"startDate": "`+start.Add(time.Hour).Format(time.RFC3339)+`", "notifyTicketHolders": true}`)
	n, told, digests := row(yes.ID)
	if n != 1 || !slices.Equal(told, []string{event.FieldCapacity, event.FieldDescription, event.FieldStart}) {
		t.Fatalf("rows %d, told %v", n, told)
	}
	if digests[event.FieldDescription] != firstDigest || len(digests) != 2 {
		t.Fatalf("digests %v, want the first change's", digests)
	}
}
