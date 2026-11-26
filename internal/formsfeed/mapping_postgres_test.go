package formsfeed_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/formsfeed"
)

func TestPostgresMappings(t *testing.T) {
	pool := migrated(t)
	ctx := context.Background()
	evStore := event.NewPostgresStore(pool)
	form := uuid.New()
	ev, err := evStore.Create(ctx, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB", FormURL: "https://forms.yildizskylab.com/" + form.String()})
	if err != nil {
		t.Fatal(err)
	}
	store := formsfeed.NewPostgresMappings(pool)
	if _, err := store.Get(ctx, ev.ID, form); !errors.Is(err, formsfeed.ErrMappingNotFound) {
		t.Fatalf("get before: %v", err)
	}
	who := uuid.New()
	at := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	saved, created, err := store.Put(ctx, formsfeed.Mapping{EventID: ev.ID, FormID: form, FirstName: " q1", LastName: "q2", Email: "q3", UpdatedBy: &who, UpdatedAt: at})
	if err != nil || !created || saved.FirstName != "q1" || saved.FullName != "" || saved.UpdatedBy == nil || *saved.UpdatedBy != who || !saved.UpdatedAt.Equal(at) {
		t.Fatalf("put: %+v %t %v", saved, created, err)
	}
	saved, created, err = store.Put(ctx, formsfeed.Mapping{EventID: ev.ID, FormID: form, FullName: "q4", Email: "q3", UpdatedAt: at.Add(time.Hour)})
	if err != nil || created || saved.FirstName != "" || saved.LastName != "" || saved.FullName != "q4" || saved.UpdatedBy != nil {
		t.Fatalf("replace: %+v %t %v", saved, created, err)
	}
	all, err := store.ForForm(ctx, form)
	if err != nil || len(all) != 1 || all[0].EventID != ev.ID {
		t.Fatalf("for form: %+v %v", all, err)
	}
	if none, err := store.ForForm(ctx, uuid.New()); err != nil || len(none) != 0 {
		t.Fatalf("another form: %+v %v", none, err)
	}
	if _, _, err := store.Put(ctx, formsfeed.Mapping{EventID: ev.ID, FormID: form, FirstName: "q1", Email: "q3"}); err == nil {
		t.Fatal("a mapping without a last name was kept")
	}
	if ok, err := store.Delete(ctx, ev.ID, form); err != nil || !ok {
		t.Fatalf("delete: %t %v", ok, err)
	}
	if ok, err := store.Delete(ctx, ev.ID, form); err != nil || ok {
		t.Fatalf("delete again: %t %v", ok, err)
	}
}
