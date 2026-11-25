package certificate_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/certificate"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// Event templates on PostgreSQL (docs/certificate-event-templates.md): the
// store keeps the Event; one current Event template per Event (an archived
// one leaves room); the template list leaves them out and ?eventId= finds
// the Event's; a member who edits the Event creates it from the club
// default and publishing binds the Event.
func TestPostgresEventTemplates(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	member := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ORGANIZASYON/GECEKODU"}}
	users := user.NewPostgresStore(pool)
	if _, _, err := user.NewService(users).Ensure(ctx, uuid.MustParse(member.ID), user.Profile{
		Email: "gecekodu@example.com", FirstName: "Gece", LastName: "Kodu", Username: "gecekodu",
	}); err != nil {
		t.Fatal(err)
	}
	events := event.NewPostgresStore(pool)
	ev, err := events.Create(ctx, event.Event{Name: "Gece Kodu 2026", Location: "YTÜ", OwnerTeam: "GECEKODU"})
	if err != nil {
		t.Fatal(err)
	}
	store := certificate.NewPostgresStore(pool)
	svc := certificate.NewServiceWithOptions(store, ticket.NewPostgresStore(pool), events, users, authz.NewAuthorizer(authz.DefaultPolicy()), &recRender{}, &recMail{}, certificate.Options{
		PublicAPIOrigin: "https://api.example.test", VerifyOrigin: "https://skyl.app/c",
	})

	item, err := svc.CreateEventTemplate(ctx, member, ev.ID, certificate.EventTemplateInput{From: certificate.EventTemplateFromCurrent})
	if err != nil {
		t.Fatal(err)
	}
	stored, err := store.GetTemplate(ctx, item.ID)
	if err != nil || stored.EventID == nil || *stored.EventID != ev.ID || stored.OwnerTeam != "GECEKODU" || len(stored.DraftLayout.Elements) == 0 {
		t.Fatalf("stored %+v %v", stored, err)
	}
	// A second current one: the index refuses it.
	second := certificate.Template{ID: uuid.New(), Name: "İkinci", OwnerTeam: "GECEKODU", EventID: &ev.ID, SourceKind: "sky", DraftLayout: versionedLayout("İKİNCİ")}
	if _, err := store.CreateTemplate(ctx, second); !errors.Is(err, certificate.ErrConflict) {
		t.Fatalf("second current: %v", err)
	}
	var exists *certificate.EventTemplateExistsError
	if _, err := svc.CreateEventTemplate(ctx, member, ev.ID, certificate.EventTemplateInput{From: "blank"}); !errors.As(err, &exists) || exists.TemplateID != item.ID {
		t.Fatalf("second through the service: %v", err)
	}

	all, err := store.ListTemplates(ctx, certificate.TemplateFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, listed := range all {
		if listed.EventID != nil {
			t.Fatalf("Event template %s in the list", listed.ID)
		}
	}
	own, err := store.ListTemplates(ctx, certificate.TemplateFilter{EventID: &ev.ID})
	if err != nil || len(own) != 1 || own[0].ID != item.ID {
		t.Fatalf("event's: %+v %v", own, err)
	}

	if _, err := svc.PublishTemplate(ctx, member, item.ID); err != nil {
		t.Fatal(err)
	}
	binding, err := store.GetBinding(ctx, certificate.ScopeEvent, ev.ID.String())
	if err != nil || binding.TemplateID != item.ID || binding.UpdatedBy == nil || binding.UpdatedBy.String() != member.ID {
		t.Fatalf("binding %+v %v", binding, err)
	}

	// Archived, it leaves room for a new one.
	if _, err := pool.Exec(ctx, `UPDATE certificate_templates SET archived_at = now() WHERE id = $1`, item.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateTemplate(ctx, second); err != nil {
		t.Fatalf("after archive: %v", err)
	}
}
