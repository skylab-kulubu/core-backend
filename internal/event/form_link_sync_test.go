package event_test

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
)

type recordedSync struct {
	mu    sync.Mutex
	calls [][]event.FormLink
}

func (r *recordedSync) SyncEventForms(_ context.Context, _ uuid.UUID, links []event.FormLink) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, links)
	return nil
}

func (r *recordedSync) last(t *testing.T, calls int) []event.FormLink {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.calls) != calls {
		t.Fatalf("sync calls = %d, want %d", len(r.calls), calls)
	}
	return r.calls[len(r.calls)-1]
}

func TestServiceSyncsFormLinksWhenAnEventIsSavedArchivedAndRestored(t *testing.T) {
	t.Parallel()
	links := &recordedSync{}
	svc := event.NewServiceWithOptions(event.NewMemoryStore(), authz.NewAuthorizer(authz.DefaultPolicy()), event.ServiceOptions{FormLinks: links})
	ctx := context.Background()
	leader := authz.Principal{ID: "l", Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
	apply := uuid.New()
	feedback := uuid.New()

	created, err := svc.Create(ctx, leader, event.Event{
		Name:      "Yaz Kampı",
		Location:  "YTÜ",
		OwnerTeam: "WEBLAB",
		FormURL:   "https://forms.yildizskylab.com/" + apply.String(),
		FormAlias: "yazkampi",
		ExtraFormURLs: []event.EventFormLink{
			{Label: "Geri bildirim", URL: "https://forms.yildizskylab.com/" + feedback.String() + "?ref=site", Alias: "geri"},
			{Label: "Site", URL: "https://yildizskylab.com/etkinlik"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	saved := links.last(t, 1)
	if len(saved) != 2 {
		t.Fatalf("links %+v", saved)
	}
	if saved[0].FormID != apply || saved[0].Alias != "yazkampi" || saved[0].Label != "Yaz Kampı" {
		t.Fatalf("apply link %+v", saved[0])
	}
	if saved[1].FormID != feedback || saved[1].Alias != "geri" || saved[1].Label != "Yaz Kampı · Geri bildirim" {
		t.Fatalf("extra link %+v", saved[1])
	}

	if err := svc.Delete(ctx, leader, created.ID); err != nil {
		t.Fatal(err)
	}
	if archived := links.last(t, 2); len(archived) != 0 {
		t.Fatalf("archiving must release every link, got %+v", archived)
	}
	if _, err := svc.Restore(ctx, leader, created.ID); err != nil {
		t.Fatal(err)
	}
	if restored := links.last(t, 3); len(restored) != 2 {
		t.Fatalf("restored links %+v", restored)
	}
}

func TestFormIDFromURLFindsTheFormSegment(t *testing.T) {
	t.Parallel()
	id := uuid.New()
	cases := map[string]bool{
		"https://forms.yildizskylab.com/" + id.String():                       true,
		"forms.yildizskylab.com/" + id.String() + "/":                         true,
		"https://forms.yildizskylab.com/admin/forms/" + id.String() + "/edit": true,
		"https://yildizskylab.com/etkinlik":                                   false,
		"":                                                                    false,
	}
	for raw, found := range cases {
		got, ok := event.FormIDFromURL(raw)
		if ok != found || (found && got != id) {
			t.Errorf("%q: got %s %v", raw, got, ok)
		}
	}
}
