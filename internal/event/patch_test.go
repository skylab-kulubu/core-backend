package event_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
)

func patchOf(t *testing.T, body string) event.Patch {
	t.Helper()
	var p event.Patch
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// A body that sends no form, alias or name leaves the form short links
// alone: binding them again would take back a link another Event took over.
func TestPatchBindsFormLinksOnlyWhenItSendsThem(t *testing.T) {
	t.Parallel()
	links := &recordedSync{}
	svc := event.NewServiceWithOptions(event.NewMemoryStore(), authz.NewAuthorizer(authz.DefaultPolicy()), event.ServiceOptions{FormLinks: links})
	ctx := context.Background()
	leader := authz.Principal{ID: "l", Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
	apply := uuid.New()
	created, err := svc.Create(ctx, leader, event.Event{
		Name: "Yaz Kampı", Location: "YTÜ", OwnerTeam: "WEBLAB",
		FormURL: "https://forms.yildizskylab.com/" + apply.String(), FormAlias: "yazkampi",
	})
	if err != nil {
		t.Fatal(err)
	}
	links.last(t, 1)

	updated, err := svc.Patch(ctx, leader, created.ID, patchOf(t, `{"capacity":40,"ranked":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if updated.Capacity != 40 || !updated.Ranked || updated.FormAlias != "yazkampi" || updated.FormURL != created.FormURL {
		t.Fatalf("updated %+v", updated)
	}
	links.last(t, 1)

	if _, err := svc.Patch(ctx, leader, created.ID, patchOf(t, `{"name":"Yaz Kampı 2"}`)); err != nil {
		t.Fatal(err)
	}
	if renamed := links.last(t, 2); len(renamed) != 1 || renamed[0].Label != "Yaz Kampı 2" || renamed[0].Alias != "yazkampi" {
		t.Fatalf("renamed links %+v", renamed)
	}

	if _, err := svc.Patch(ctx, leader, created.ID, patchOf(t, `{"formUrl":null}`)); err != nil {
		t.Fatal(err)
	}
	if cleared := links.last(t, 3); len(cleared) != 0 {
		t.Fatalf("cleared links %+v", cleared)
	}
}

func TestPatchChecksRightsBeforeTheBody(t *testing.T) {
	t.Parallel()
	svc := event.NewService(event.NewMemoryStore(), authz.NewAuthorizer(authz.DefaultPolicy()))
	ctx := context.Background()
	leader := authz.Principal{ID: "l", Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
	member := authz.Principal{ID: "m", Groups: []string{"/UYELER/ARGE/WEBLAB"}}
	created, err := svc.Create(ctx, leader, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Patch(ctx, member, created.ID, patchOf(t, `{"name":""}`)); !errors.Is(err, event.ErrForbidden) {
		t.Fatalf("member: %v", err)
	}
	if _, err := svc.Patch(ctx, leader, created.ID, patchOf(t, `{"name":""}`)); !errors.Is(err, event.ErrInvalid) {
		t.Fatalf("leader: %v", err)
	}
	if _, err := svc.Patch(ctx, leader, uuid.New(), patchOf(t, `{}`)); !errors.Is(err, event.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
}
