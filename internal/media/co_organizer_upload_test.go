package media_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

// seats seats the ids it holds and counts its reads.
type seats struct {
	ids   map[string]bool
	reads atomic.Int64
}

func (s *seats) HasCoOrganizerSeat(_ context.Context, p authz.Principal) bool {
	s.reads.Add(1)
	return s.ids[p.ID]
}

// An Event's cover (event_editor) is uploaded by who holds a Co-organizer
// seat, though they may create an Event for no team of theirs; not by who
// holds none. The seat is read only when the caller's own teams refuse
// them, and never for a purpose of another rule (docs/co-organizers.md).
func TestCoOrganizerSeatMeetsTheEventEditorRule(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	seated := signedIn("60606060-6060-6060-6060-606060606060")
	unseated := signedIn("61616161-6161-6161-6161-616161616161")
	s := &seats{ids: map[string]bool{seated.ID: true}}
	svc := media.NewServiceWithOptions(media.NewMemoryStore(), media.NewMemoryBlob(), authz.NewAuthorizer(authz.DefaultPolicy()), "https://cdn.example.test",
		media.ServiceOptions{CoOrganizerSeats: s})
	if _, err := svc.UploadForPurpose(ctx, seated, "event_cover", uploaded("dot.png", "image/png", pngDot())); err != nil {
		t.Fatalf("a seated person uploads a cover: %v", err)
	}
	if _, err := svc.UploadForPurpose(ctx, unseated, "event_cover", uploaded("dot.png", "image/png", pngDot())); !errors.Is(err, media.ErrPurposeForbidden) {
		t.Fatalf("a person with no seat uploads a cover: %v", err)
	}
	s.reads.Store(0)
	leader := authz.Principal{ID: seated.ID, Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
	if _, err := svc.UploadForPurpose(ctx, leader, "event_cover", uploaded("dot.png", "image/png", pngDot())); err != nil {
		t.Fatalf("WEBLAB's Leader uploads a cover: %v", err)
	}
	if _, err := svc.UploadForPurpose(ctx, unseated, "profile_picture", uploaded("dot.png", "image/png", pngDot())); err != nil {
		t.Fatalf("a profile picture: %v", err)
	}
	if n := s.reads.Load(); n != 0 {
		t.Fatalf("the seat read %d times for callers their own rules let in", n)
	}
	without := media.NewService(media.NewMemoryStore(), media.NewMemoryBlob(), authz.NewAuthorizer(authz.DefaultPolicy()), "https://cdn.example.test")
	if _, err := without.UploadForPurpose(ctx, seated, "event_cover", uploaded("dot.png", "image/png", pngDot())); !errors.Is(err, media.ErrPurposeForbidden) {
		t.Fatalf("without the seats a person uploads a cover: %v", err)
	}
}
