package certificate_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/certificate"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// Attendance finalization opens when the Event is published and has ended,
// or is undated (ADR 0066); a draft and a cancelled Event refuse it with
// their own reason, a running one with a plain conflict.
func TestFinalizeFollowsTheEventStatusAndEnd(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	past := func(h int) *time.Time { v := time.Now().Add(-time.Duration(h) * time.Hour); return &v }
	future := func(h int) *time.Time { v := time.Now().Add(time.Duration(h) * time.Hour); return &v }
	cases := []struct {
		name       string
		status     event.Status
		start, end *time.Time
		want       error // nil: finalization runs
		reason     error // the event status error it carries, if any
	}{
		{"published ended", event.StatusPublished, past(48), past(40), nil, nil},
		{"published undated", event.StatusPublished, nil, nil, nil, nil},
		{"published start only, ended", event.StatusPublished, past(30), nil, nil, nil},
		{"published running", event.StatusPublished, past(1), future(5), certificate.ErrConflict, nil},
		{"published upcoming", event.StatusPublished, future(24), future(30), certificate.ErrConflict, nil},
		{"draft ended", event.StatusDraft, past(48), past(40), certificate.ErrConflict, event.ErrNotPublished},
		{"draft undated", event.StatusDraft, nil, nil, certificate.ErrConflict, event.ErrNotPublished},
		{"cancelled ended", event.StatusCancelled, past(48), past(40), certificate.ErrConflict, event.ErrCancelled},
		{"cancelled undated", event.StatusCancelled, nil, nil, certificate.ErrConflict, event.ErrCancelled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			events := event.NewMemoryStore()
			tickets := ticket.NewMemoryStore()
			users := user.NewMemoryStore()
			store := certificate.NewMemoryStore()
			authorizer := authz.NewAuthorizer(authz.DefaultPolicy())
			service := certificate.NewServiceWithOptions(store, tickets, events, users, authorizer, &recRender{}, &recMail{}, certificate.Options{
				PublicAPIOrigin: "https://api.example.test", VerifyOrigin: "https://skyl.app/c", Templates: store, Jobs: store,
			})
			template, err := store.CreateTemplate(ctx, certificate.Template{ID: uuid.New(), Name: "ARTLAB", OwnerTeam: "ARTLAB", SourceKind: "figma", DraftLayout: versionedLayout("ARTLAB")})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.CreateVersion(ctx, certificate.TemplateVersion{ID: uuid.New(), TemplateID: template.ID, Layout: template.DraftLayout, Checksum: "one"}); err != nil {
				t.Fatal(err)
			}
			if _, err := store.SetBinding(ctx, certificate.Binding{ID: uuid.New(), Scope: certificate.ScopeOwnerTeam, ScopeKey: "ARTLAB", TemplateID: template.ID}); err != nil {
				t.Fatal(err)
			}
			ev, err := events.Create(ctx, event.Event{Name: tc.name, Location: "YTÜ", OwnerTeam: "ARTLAB", AttendanceRule: certificate.RuleOnce,
				Status: tc.status, StartDate: tc.start, EndDate: tc.end})
			if err != nil {
				t.Fatal(err)
			}
			leader := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/ARTLAB/LIDERLER"}}
			_, err = service.Finalize(ctx, leader, ev.ID)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("finalize: %v", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("finalize: %v, want %v", err, tc.want)
			}
			if tc.reason != nil && !errors.Is(err, tc.reason) {
				t.Fatalf("finalize: %v, want the reason %v", err, tc.reason)
			}
			if tc.reason == nil && (errors.Is(err, event.ErrNotPublished) || errors.Is(err, event.ErrCancelled)) {
				t.Fatalf("finalize: %v carries a status reason", err)
			}
		})
	}
}
