package event_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/optional"
)

// A Session's feedback form (etkinlik geri bildirim formu, ticket 03).

type sessionFeedbackEnv struct {
	store *event.MemoryStore
	links *recordedSync
	svc   event.Service
	ev    event.Event
	day   event.Day
}

func newSessionFeedbackEnv(t *testing.T) sessionFeedbackEnv {
	t.Helper()
	ctx := context.Background()
	env := sessionFeedbackEnv{store: event.NewMemoryStore(), links: &recordedSync{}}
	env.svc = event.NewServiceWithOptions(env.store, authz.NewAuthorizer(authz.DefaultPolicy()), event.ServiceOptions{FormLinks: env.links})
	var err error
	env.ev, err = env.svc.Create(ctx, copyLeader, event.Event{Name: "Gecekodu", Location: "YTÜ", OwnerTeam: "WEBLAB", FeedbackMode: event.FeedbackPerSession})
	if err != nil {
		t.Fatal(err)
	}
	if env.day, err = env.svc.CreateDay(ctx, copyLeader, event.Day{EventID: env.ev.ID, Name: "Cumartesi"}); err != nil {
		t.Fatal(err)
	}
	return env
}

func (env sessionFeedbackEnv) session(t *testing.T, title, form string) event.Session {
	t.Helper()
	sess, err := env.svc.CreateSession(context.Background(), copyLeader, event.Session{
		EventDayID: env.day.ID, Title: title, SpeakerName: "Ada", SessionType: "talk",
		FeedbackFormURL: form, FeedbackFormAlias: "gb-" + title, FeedbackAutoSend: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return sess
}

// syncedForms are the forms of the last sync, by label.
func (env sessionFeedbackEnv) syncedForms(t *testing.T) map[string]uuid.UUID {
	t.Helper()
	env.links.mu.Lock()
	defer env.links.mu.Unlock()
	out := map[string]uuid.UUID{}
	if len(env.links.calls) > 0 {
		for _, link := range env.links.calls[len(env.links.calls)-1] {
			out[link.Label] = link.FormID
		}
	}
	return out
}

func TestSessionFeedbackFormKeptLinkedAndReleased(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newSessionFeedbackEnv(t)
	form := uuid.New()
	sess := env.session(t, "Açılış", formURL(form))
	if sess.FeedbackFormURL != formURL(form) || sess.FeedbackFormAlias != "gb-Açılış" || !sess.FeedbackAutoSend {
		t.Fatalf("created %+v", sess)
	}
	if got := env.syncedForms(t); got["Gecekodu · Açılış · Geri bildirim"] != form {
		t.Fatalf("synced %v", got)
	}
	// An update that sends nothing of the form keeps it.
	kept, err := env.svc.UpdateSession(ctx, copyLeader, sess.ID, event.Session{Title: "Açılış", SpeakerName: "Can", SessionType: "talk"}, nil, event.SessionFeedback{})
	if err != nil || kept.FeedbackFormURL != formURL(form) || kept.FeedbackFormAlias != "gb-Açılış" || !kept.FeedbackAutoSend || kept.SpeakerName != "Can" {
		t.Fatalf("kept %+v %v", kept, err)
	}
	off := false
	kept, err = env.svc.UpdateSession(ctx, copyLeader, sess.ID, kept, nil, event.SessionFeedback{AutoSend: optional.Field[bool]{Set: true, Value: off}})
	if err != nil || kept.FeedbackAutoSend || kept.FeedbackFormURL == "" {
		t.Fatalf("auto send off %+v %v", kept, err)
	}
	// Renaming the Session relabels its link.
	kept, err = env.svc.UpdateSession(ctx, copyLeader, sess.ID, event.Session{Title: "Giriş", SpeakerName: "Can", SessionType: "talk"}, nil, event.SessionFeedback{})
	if err != nil {
		t.Fatal(err)
	}
	if got := env.syncedForms(t); got["Gecekodu · Giriş · Geri bildirim"] != form {
		t.Fatalf("relabelled %v", got)
	}
	// Saving the Event keeps its Sessions' links.
	if _, err := env.svc.Patch(ctx, copyLeader, env.ev.ID, patchOf(t, `{"name":"Gecekodu 2026"}`)); err != nil {
		t.Fatal(err)
	}
	if got := env.syncedForms(t); got["Gecekodu 2026 · Giriş · Geri bildirim"] != form {
		t.Fatalf("event saved %v", got)
	}
	// Archiving the Session releases its link; restoring binds it again.
	if err := env.svc.DeleteSession(ctx, copyYK, sess.ID); err != nil {
		t.Fatal(err)
	}
	if got := env.syncedForms(t); len(got) != 0 {
		t.Fatalf("archived session %v", got)
	}
	if _, err := env.svc.RestoreSession(ctx, copyYK, sess.ID); err != nil {
		t.Fatal(err)
	}
	if got := env.syncedForms(t); got["Gecekodu 2026 · Giriş · Geri bildirim"] != form {
		t.Fatalf("restored %v", got)
	}
	// So does archiving its day.
	if err := env.svc.DeleteDay(ctx, copyYK, env.day.ID); err != nil {
		t.Fatal(err)
	}
	if got := env.syncedForms(t); len(got) != 0 {
		t.Fatalf("archived day %v", got)
	}
	if _, err := env.svc.RestoreDay(ctx, copyYK, env.day.ID); err != nil {
		t.Fatal(err)
	}
	if got := env.syncedForms(t); len(got) != 1 {
		t.Fatalf("restored day %v", got)
	}
	// Clearing the form releases it.
	empty := ""
	cleared, err := env.svc.UpdateSession(ctx, copyLeader, sess.ID, kept, nil, event.SessionFeedback{FormURL: optional.Field[string]{Set: true, Value: empty}, FormAlias: optional.Field[string]{Set: true, Null: true}})
	if err != nil || cleared.FeedbackFormURL != "" || cleared.FeedbackFormAlias != "" {
		t.Fatalf("cleared %+v %v", cleared, err)
	}
	if got := env.syncedForms(t); len(got) != 0 {
		t.Fatalf("cleared %v", got)
	}
}

// One place per feedback form, Sessions' included.
func TestSessionFeedbackFormOnePlace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newSessionFeedbackEnv(t)
	apply, eventFeedback, sessionForm := uuid.New(), uuid.New(), uuid.New()
	if _, err := env.svc.Patch(ctx, copyLeader, env.ev.ID, patchOf(t, `{"formUrl":"`+formURL(apply)+`","feedbackFormUrl":"`+formURL(eventFeedback)+`"}`)); err != nil {
		t.Fatal(err)
	}
	first := env.session(t, "Açılış", formURL(sessionForm))
	other, err := env.svc.Create(ctx, copyLeader, event.Event{Name: "Başka", Location: "YTÜ", OwnerTeam: "WEBLAB"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		form uuid.UUID
	}{
		{"its Event's application form", apply},
		{"its Event's feedback form", eventFeedback},
		{"another Session's form", sessionForm},
	} {
		if _, err := env.svc.CreateSession(ctx, copyLeader, event.Session{EventDayID: env.day.ID, Title: "x", SpeakerName: "Ada", SessionType: "talk", FeedbackFormURL: formURL(tc.form)}); !errors.Is(err, event.ErrFormInUse) {
			t.Errorf("create, %s: %v", tc.name, err)
		}
	}
	second := env.session(t, "Kapanış", "")
	url := formURL(sessionForm)
	if _, err := env.svc.UpdateSession(ctx, copyLeader, second.ID, second, nil, event.SessionFeedback{FormURL: optional.Field[string]{Set: true, Value: url}}); !errors.Is(err, event.ErrFormInUse) {
		t.Fatalf("update: %v", err)
	}
	// The Session keeps its own form on an update.
	if _, err := env.svc.UpdateSession(ctx, copyLeader, first.ID, first, nil, event.SessionFeedback{FormURL: optional.Field[string]{Set: true, Value: url}}); err != nil {
		t.Fatalf("own form: %v", err)
	}
	for _, body := range []string{`{"feedbackFormUrl":"` + url + `"}`, `{"formUrl":"` + url + `"}`, `{"extraFormUrls":[{"label":"x","url":"` + url + `"}]}`} {
		if _, err := env.svc.Patch(ctx, copyLeader, other.ID, patchOf(t, body)); !errors.Is(err, event.ErrFormInUse) {
			t.Errorf("another Event %s: %v", body, err)
		}
	}
	// Archived, the Session frees its form; restored after it was taken,
	// it is refused.
	if err := env.svc.DeleteSession(ctx, copyYK, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.svc.UpdateSession(ctx, copyLeader, second.ID, second, nil, event.SessionFeedback{FormURL: optional.Field[string]{Set: true, Value: url}}); err != nil {
		t.Fatalf("after archive: %v", err)
	}
	if _, err := env.svc.RestoreSession(ctx, copyYK, first.ID); !errors.Is(err, event.ErrFormInUse) {
		t.Fatalf("restore: %v", err)
	}
}

// A copied Session starts without a feedback form; its box comes along
// (review B4).
func TestCopyResetsSessionFeedback(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newSessionFeedbackEnv(t)
	sess := env.session(t, "Açılış", formURL(uuid.New()))
	off := false
	if _, err := env.svc.UpdateSession(ctx, copyLeader, sess.ID, sess, nil, event.SessionFeedback{AutoSend: optional.Field[bool]{Set: true, Value: off}}); err != nil {
		t.Fatal(err)
	}
	got, _, err := env.svc.Copy(ctx, copyLeader, env.ev.ID, copyIn("Kopya", "2027-01-01T10:00:00+03:00"))
	if err != nil || got.Event.FeedbackMode != event.FeedbackPerSession {
		t.Fatalf("copy %+v %v", got.Event, err)
	}
	days, _ := env.store.ListDays(ctx, got.Event.ID)
	sessions, _ := env.store.ListSessions(ctx, days[0].ID)
	if len(sessions) != 1 || sessions[0].FeedbackFormURL != "" || sessions[0].FeedbackFormAlias != "" || sessions[0].FeedbackAutoSend {
		t.Fatalf("copied sessions %+v", sessions)
	}
}
