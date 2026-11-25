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
	"github.com/skylab-kulubu/core-backend/internal/user"
)

type noticeChoice struct {
	decidedBy *uuid.UUID
	fields    []string
	notify    *bool
	queued    bool
}

// The editor's answer to "tell the Ticket holders?" (notifyTicketHolders,
// docs/event-updated-notification.md#the-editors-choice): no queues nothing,
// yes and a body without the field queue as before; within the window a no
// leaves the waiting notice as it is and its field untold, a later yes adds
// its field. Every answer about a change of time or place of a published
// Event is recorded, while the Notification is off too; a change of
// anything else records nothing.
func TestTheEditorsAnswerDecidesAndIsRecorded(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := event.NewPostgresStore(pool)
	svc := event.NewService(store, authz.NewAuthorizer(authz.DefaultPolicy()))
	editorID := uuid.New()
	if _, _, err := user.NewService(user.NewPostgresStore(pool)).Ensure(ctx, editorID, user.Profile{Email: "duzenleyen@example.com"}); err != nil {
		t.Fatal(err)
	}
	editor := authz.Principal{ID: editorID.String(), Groups: copyLeader.Groups}
	start := time.Now().Add(72 * time.Hour).UTC().Truncate(time.Second)
	published := func() event.Event {
		t.Helper()
		e, err := svc.Create(ctx, editor, event.Event{Name: "Gece Kodu", Location: "D-Blok", OwnerTeam: "WEBLAB", Status: event.StatusPublished, StartDate: &start})
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	patch := func(p authz.Principal, id uuid.UUID, body string) {
		t.Helper()
		var b event.Patch
		if err := json.Unmarshal([]byte(body), &b); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.Patch(ctx, p, id, b); err != nil {
			t.Fatalf("patch %s: %v", body, err)
		}
	}
	choices := func(id uuid.UUID) []noticeChoice {
		t.Helper()
		rows, err := pool.Query(ctx, `SELECT decided_by, fields, notify, queued FROM event_change_notice_choices WHERE event_id = $1 ORDER BY decided_at, id`, id)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []noticeChoice
		for rows.Next() {
			var c noticeChoice
			if err := rows.Scan(&c.decidedBy, &c.fields, &c.notify, &c.queued); err != nil {
				t.Fatal(err)
			}
			out = append(out, c)
		}
		return out
	}
	waiting := func(id uuid.UUID) (int, []string) {
		t.Helper()
		var n int
		var told []string
		if err := pool.QueryRow(ctx, `SELECT count(*), (SELECT told_fields FROM event_update_notifications WHERE event_id = $1 LIMIT 1)
			FROM event_update_notifications WHERE event_id = $1`, id).Scan(&n, &told); err != nil {
			t.Fatal(err)
		}
		return n, told
	}
	later := func(hours int) string { return start.Add(time.Duration(hours) * time.Hour).Format(time.RFC3339) }

	// Off: nothing is queued, the answer is recorded.
	off := published()
	patch(editor, off.ID, `{"location": "Oditoryum", "notifyTicketHolders": true}`)
	if n, _ := waiting(off.ID); n != 0 {
		t.Fatalf("queued %d while off", n)
	}
	if got := choices(off.ID); len(got) != 1 || got[0].decidedBy == nil || *got[0].decidedBy != editorID ||
		!slices.Equal(got[0].fields, []string{event.FieldLocation}) || got[0].notify == nil || !*got[0].notify || got[0].queued {
		t.Fatalf("choices %+v", got)
	}

	store.NotifyUpdated(true)

	// No: nothing is queued.
	no := published()
	patch(editor, no.ID, `{"startDate": "`+later(1)+`", "notifyTicketHolders": false}`)
	if n, _ := waiting(no.ID); n != 0 {
		t.Fatalf("queued %d after no", n)
	}
	if got := choices(no.ID); len(got) != 1 || !slices.Equal(got[0].fields, []string{event.FieldStart}) || got[0].notify == nil || *got[0].notify || got[0].queued {
		t.Fatalf("choices after no %+v", got)
	}

	// A body without the field (an older client) tells, as before; a
	// principal without a users row is recorded as nobody.
	silent := published()
	patch(copyLeader, silent.ID, `{"location": "Oditoryum"}`)
	if n, told := waiting(silent.ID); n != 1 || !slices.Equal(told, []string{event.FieldLocation}) {
		t.Fatalf("waiting %d %v after a body without the answer", n, told)
	}
	if got := choices(silent.ID); len(got) != 1 || got[0].decidedBy != nil || got[0].notify != nil || !got[0].queued {
		t.Fatalf("choices after a body without the answer %+v", got)
	}

	// Within the window: yes (start), no (location) leaves the location
	// untold, yes (location) adds it.
	mixed := published()
	patch(editor, mixed.ID, `{"startDate": "`+later(2)+`", "notifyTicketHolders": true}`)
	patch(editor, mixed.ID, `{"location": "Kongre Merkezi", "notifyTicketHolders": false}`)
	if n, told := waiting(mixed.ID); n != 1 || !slices.Equal(told, []string{event.FieldStart}) {
		t.Fatalf("waiting %d %v after yes then no", n, told)
	}
	patch(editor, mixed.ID, `{"location": "Oditoryum", "notifyTicketHolders": true}`)
	if n, told := waiting(mixed.ID); n != 1 || !slices.Equal(told, []string{event.FieldLocation, event.FieldStart}) {
		t.Fatalf("waiting %d %v after yes, no, yes", n, told)
	}
	var beforeLocation string
	if err := pool.QueryRow(ctx, `SELECT before_location FROM event_update_notifications WHERE event_id = $1`, mixed.ID).Scan(&beforeLocation); err != nil || beforeLocation != "D-Blok" {
		t.Fatalf("before location %q %v: the first change's before stays", beforeLocation, err)
	}
	if got := choices(mixed.ID); len(got) != 3 || !got[0].queued || got[1].queued || !got[2].queued {
		t.Fatalf("choices within the window %+v", got)
	}

	// A change of anything else records nothing; null is refused.
	other := published()
	patch(editor, other.ID, `{"name": "Gece Kodu 2026", "description": "Yeni", "notifyTicketHolders": true}`)
	if got := choices(other.ID); len(got) != 0 {
		t.Fatalf("choices for a new name %+v", got)
	}
	var b event.Patch
	if err := json.Unmarshal([]byte(`{"location": "X", "notifyTicketHolders": null}`), &b); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Patch(ctx, editor, other.ID, b); err == nil {
		t.Fatal("notifyTicketHolders null was taken")
	}

	// A draft's change is no news: nothing queued, nothing recorded.
	draft, err := svc.Create(ctx, editor, event.Event{Name: "Taslak", Location: "D-Blok", OwnerTeam: "WEBLAB", Status: event.StatusDraft, StartDate: &start})
	if err != nil {
		t.Fatal(err)
	}
	patch(editor, draft.ID, `{"location": "Oditoryum", "notifyTicketHolders": true}`)
	if n, _ := waiting(draft.ID); n != 0 || len(choices(draft.ID)) != 0 {
		t.Fatalf("a draft's change: waiting %d, choices %+v", n, choices(draft.ID))
	}
}
