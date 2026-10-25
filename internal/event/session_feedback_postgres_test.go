package event_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
)

// The store writes and reads a Session's feedback form; the list of
// Session forms leaves out archived Sessions and days, and an archived
// Event's unless asked for it; a copied Session has none.
func TestPostgresStoreSessionFeedbackForms(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := event.NewPostgresStore(pool)
	ev, err := store.Create(ctx, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB", StartDate: stamp("2026-11-01T10:00:00+03:00")})
	if err != nil {
		t.Fatal(err)
	}
	day, err := store.CreateDay(ctx, event.Day{EventID: ev.ID, Name: "Gün"})
	if err != nil {
		t.Fatal(err)
	}
	gone, err := store.CreateDay(ctx, event.Day{EventID: ev.ID, Name: "Silinen gün"})
	if err != nil {
		t.Fatal(err)
	}
	forms := map[string]uuid.UUID{}
	sessions := map[string]event.Session{}
	for _, c := range []struct {
		title string
		day   uuid.UUID
	}{{"Açılış", day.ID}, {"Arşiv", day.ID}, {"Silinen günde", gone.ID}, {"Formsuz", day.ID}} {
		in := event.Session{EventDayID: c.day, Title: c.title, SpeakerName: "Ada", SessionType: "talk", FeedbackAutoSend: c.title != "Açılış"}
		if c.title != "Formsuz" {
			forms[c.title] = uuid.New()
			in.FeedbackFormURL, in.FeedbackFormAlias = formURL(forms[c.title]), "gb-"+c.title
		}
		created, err := store.CreateSession(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		sessions[c.title] = created
	}
	if s := sessions["Açılış"]; s.FeedbackFormURL != formURL(forms["Açılış"]) || s.FeedbackFormAlias != "gb-Açılış" || s.FeedbackAutoSend {
		t.Fatalf("created %+v", s)
	}
	s := sessions["Açılış"]
	s.FeedbackAutoSend, s.FeedbackFormAlias = true, "yeni"
	if updated, err := store.UpdateSession(ctx, s); err != nil || !updated.FeedbackAutoSend || updated.FeedbackFormAlias != "yeni" {
		t.Fatalf("updated %+v %v", updated, err)
	}
	if err := store.ArchiveSession(ctx, sessions["Arşiv"].ID, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.ArchiveDay(ctx, gone.ID, nil); err != nil {
		t.Fatal(err)
	}
	only := func(got []event.SessionFeedbackForm, err error) {
		t.Helper()
		if err != nil || len(got) != 1 || got[0].SessionID != s.ID || got[0].EventID != ev.ID || got[0].Title != "Açılış" || got[0].Alias != "yeni" {
			t.Fatalf("forms %+v %v", got, err)
		}
	}
	only(store.ListSessionFeedbackForms(ctx, uuid.Nil))
	only(store.ListSessionFeedbackForms(ctx, ev.ID))
	if err := store.Archive(ctx, ev.ID, nil); err != nil {
		t.Fatal(err)
	}
	if got, err := store.ListSessionFeedbackForms(ctx, uuid.Nil); err != nil || len(got) != 0 {
		t.Fatalf("archived Event in the global list: %+v %v", got, err)
	}
	only(store.ListSessionFeedbackForms(ctx, ev.ID))
	if err := store.Restore(ctx, ev.ID); err != nil {
		t.Fatal(err)
	}
	svc := event.NewService(store, authz.NewAuthorizer(authz.DefaultPolicy()))
	// The box comes along (review B4): Formsuz is not mailed, Açılış is.
	if _, err := pool.Exec(ctx, `UPDATE sessions SET feedback_auto_send = false WHERE id = $1`, sessions["Formsuz"].ID); err != nil {
		t.Fatal(err)
	}
	copied, _, err := svc.Copy(ctx, copyLeader, ev.ID, copyIn("Kopya", "2027-11-01T10:00:00+03:00"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := store.ListSessionFeedbackForms(ctx, copied.Event.ID); err != nil || len(got) != 0 {
		t.Fatalf("copy has forms %+v %v", got, err)
	}
	var autoSend int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sessions s JOIN event_days d ON d.id = s.event_day_id WHERE d.event_id = $1 AND s.feedback_auto_send`, copied.Event.ID).Scan(&autoSend); err != nil || autoSend != 1 {
		t.Fatalf("copied sessions mailed: %d %v", autoSend, err)
	}
}
