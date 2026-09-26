package shorturl_test

import (
	"context"
	"errors"
	"io/fs"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/db"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/shorturl"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

func formsURL(id uuid.UUID) string {
	return "https://forms.yildizskylab.com/" + id.String()
}

func TestFormLinksOnPostgres(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	users := user.NewService(user.NewPostgresStore(pool))
	serviceID := uuid.New()
	actorID := uuid.New()
	for id, email := range map[uuid.UUID]string{serviceID: "forms-service@example.test", actorID: "actor@example.test"} {
		if _, _, err := users.Ensure(ctx, id, user.Profile{Email: email}); err != nil {
			t.Fatal(err)
		}
	}
	forms := authz.Principal{ID: serviceID.String(), Roles: []string{"url:forms"}}
	owner := authz.Principal{ID: actorID.String(), Roles: []string{"url:access"}}
	svc := shorturl.NewService(shorturl.NewPostgresStore(pool), authz.NewAuthorizer(authz.DefaultPolicy()))

	formID := uuid.New()
	link, err := svc.EnsureFormLink(ctx, forms, formID, shorturl.FormLinkInput{URL: formsURL(formID), Label: "Yaz Kampı", Alias: "yaz-kampi-2026", ActorID: &actorID})
	if err != nil {
		t.Fatal(err)
	}
	if link.Alias != "yaz-kampi-2026" || link.CreatedBy == nil || *link.CreatedBy != actorID || link.FormID == nil || *link.FormID != formID {
		t.Fatalf("link %+v", link)
	}
	stranger := uuid.New()
	otherForm := uuid.New()
	fallback, err := svc.EnsureFormLink(ctx, forms, otherForm, shorturl.FormLinkInput{URL: formsURL(otherForm), ActorID: &stranger})
	if err != nil {
		t.Fatalf("actor unknown to core: %v", err)
	}
	if fallback.CreatedBy == nil || *fallback.CreatedBy != serviceID {
		t.Fatalf("fallback creator %+v", fallback.CreatedBy)
	}

	renamed, err := svc.RenameFormLink(ctx, forms, formID, "yaz-kampi", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(ctx, owner, "https://example.com", link.Alias); !errors.Is(err, shorturl.ErrConflict) {
		t.Fatalf("retired alias reused: %v", err)
	}
	if _, err := svc.Create(ctx, owner, "https://example.com", "Yaz-Kampi"); !errors.Is(err, shorturl.ErrConflict) {
		t.Fatalf("case variant: %v", err)
	}
	if back, err := svc.RenameFormLink(ctx, forms, formID, "", "yaz-kampi-2026"); err != nil || back.Alias != link.Alias {
		t.Fatalf("take the default back: %+v %v", back, err)
	}
	if _, err := svc.RenameFormLink(ctx, forms, formID, renamed.Alias, ""); err != nil {
		t.Fatal(err)
	}

	personal, err := svc.Create(ctx, owner, formsURL(formID), "kisisel")
	if err != nil {
		t.Fatal(err)
	}
	for _, hop := range []struct {
		alias  string
		source string
	}{{"yaz-kampi", "instagram"}, {link.Alias, "instagram"}, {"yaz-kampi", "qr"}, {personal.Alias, ""}} {
		if _, err := svc.Redirect(ctx, hop.alias, shorturl.Hit{UTM: shorturl.UTM{Source: hop.source}}); err != nil {
			t.Fatal(err)
		}
	}
	stats, err := svc.FormStats(ctx, forms, formID)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Total != 4 || len(stats.Sources) != 3 || stats.Sources[0].Source != "instagram" || stats.Sources[0].Count != 2 {
		t.Fatalf("stats %+v", stats)
	}

	events := event.NewPostgresStore(pool)
	ev, err := events.Create(ctx, event.Event{Name: "Yaz Kampı 2026", Location: "YTÜ", OwnerTeam: "WEBLAB"})
	if err != nil {
		t.Fatal(err)
	}
	panelLink, err := svc.Create(ctx, owner, formsURL(formID), "yazkampi")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.SyncEventForms(ctx, ev.ID, []shorturl.EventForm{{FormID: formID, URL: formsURL(formID), Alias: "yazkampi", Label: ev.Name}}); err != nil {
		t.Fatal(err)
	}
	bound, err := svc.FormLink(ctx, forms, formID)
	if err != nil {
		t.Fatal(err)
	}
	if bound.ID != panelLink.ID || bound.EventID == nil || *bound.EventID != ev.ID || bound.Label != ev.Name {
		t.Fatalf("event link %+v", bound)
	}
	if _, err := svc.Lookup(ctx, "yaz-kampi"); err != nil {
		t.Fatalf("released form link must keep redirecting: %v", err)
	}
	if _, err := svc.RenameFormLink(ctx, forms, formID, "baska", ""); !errors.Is(err, shorturl.ErrEventManaged) {
		t.Fatalf("rename event link: %v", err)
	}
	if err := svc.SyncEventForms(ctx, ev.ID, nil); err != nil {
		t.Fatal(err)
	}
	handedBack, err := svc.FormLink(ctx, forms, formID)
	if err != nil || handedBack.EventID != nil || handedBack.ID != panelLink.ID {
		t.Fatalf("handed back %+v %v", handedBack, err)
	}
}

func TestRestoringAReplacedFormLinkConflictsOnPostgres(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	users := user.NewService(user.NewPostgresStore(pool))
	serviceID := uuid.New()
	moderatorID := uuid.New()
	for id, email := range map[uuid.UUID]string{serviceID: "forms-service@example.test", moderatorID: "moderator@example.test"} {
		if _, _, err := users.Ensure(ctx, id, user.Profile{Email: email}); err != nil {
			t.Fatal(err)
		}
	}
	forms := authz.Principal{ID: serviceID.String(), Roles: []string{"url:forms"}}
	moderator := authz.Principal{ID: moderatorID.String(), Roles: []string{"url:moderator"}}
	svc := shorturl.NewService(shorturl.NewPostgresStore(pool), authz.NewAuthorizer(authz.DefaultPolicy()))

	formID := uuid.New()
	first, err := svc.EnsureFormLink(ctx, forms, formID, shorturl.FormLinkInput{URL: formsURL(formID), Alias: "yaz-kampi-2026"})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Delete(ctx, moderator, first.ID); err != nil {
		t.Fatal(err)
	}
	second, err := svc.EnsureFormLink(ctx, forms, formID, shorturl.FormLinkInput{URL: formsURL(formID), Alias: "yaz-kampi-2026"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Restore(ctx, moderator, first.ID); !errors.Is(err, shorturl.ErrConflict) {
		t.Fatalf("restoring a form link while the form has another: %v", err)
	}
	if current, err := svc.FormLink(ctx, forms, formID); err != nil || current.ID != second.ID {
		t.Fatalf("the form keeps its current link: %+v %v", current, err)
	}
}

func TestURLFormLinksMigrationBindsExistingLinks(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	apply, feedback, single, doubled := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	ev, err := event.NewPostgresStore(pool).Create(ctx, event.Event{
		Name: "Yaz Kampı", Location: "YTÜ", OwnerTeam: "WEBLAB",
		FormURL: formsURL(apply), FormAlias: "yazkampi",
		ExtraFormURLs: []event.EventFormLink{{Label: "Geri bildirim", URL: formsURL(feedback), Alias: "geri"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	rows := []struct {
		alias  string
		url    string
		clicks int
	}{
		{"yazkampi", formsURL(apply), 0},
		{"eski", formsURL(apply), 5},
		{"geri", formsURL(feedback), 0},
		{"tek", formsURL(single) + "/", 0},
		{"cift-a", formsURL(doubled), 1},
		{"cift-b", formsURL(doubled), 9},
		{"alakasiz", "https://example.com/" + apply.String(), 0},
	}
	for _, r := range rows {
		if _, err := pool.Exec(ctx, `INSERT INTO urls (id, alias, url, click_count) VALUES ($1, $2, $3, $4)`, uuid.New(), r.alias, r.url, r.clicks); err != nil {
			t.Fatal(err)
		}
	}
	body, err := fs.ReadFile(db.UpSQL, "migrations/20260926114000_url_form_links.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(body)); err != nil {
		t.Fatalf("rerun migration: %v", err)
	}

	want := map[string]struct {
		form  *uuid.UUID
		event bool
		label string
	}{
		"yazkampi": {&apply, true, "Yaz Kampı"},
		"geri":     {&feedback, true, "Yaz Kampı · Geri bildirim"},
		"eski":     {nil, false, ""},
		"tek":      {&single, false, ""},
		"cift-a":   {nil, false, ""},
		"cift-b":   {&doubled, false, ""},
		"alakasiz": {nil, false, ""},
	}
	for alias, w := range want {
		var formID, eventID *uuid.UUID
		var label string
		if err := pool.QueryRow(ctx, `SELECT form_id, event_id, label FROM urls WHERE alias = $1`, alias).Scan(&formID, &eventID, &label); err != nil {
			t.Fatal(err)
		}
		if (w.form == nil) != (formID == nil) || (w.form != nil && *w.form != *formID) {
			t.Errorf("%s form %v", alias, formID)
		}
		if w.event != (eventID != nil) || (w.event && *eventID != ev.ID) || label != w.label {
			t.Errorf("%s event %v label %q", alias, eventID, label)
		}
	}
}
