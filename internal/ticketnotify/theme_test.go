package ticketnotify_test

import (
	"strings"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// An ARTLAB Event's ticket mail carries its theme and poster: Theme
// "artlab" and EventBannerUrl, the poster's https address
// (docs/event-theme.md#mails); the waiting list's and the cancellation's
// mails keep both.
func TestTicketMailCarriesTheEventsTheme(t *testing.T) {
	e := newEnv(t)
	key := "artlab"
	ev := e.ev
	ev.Theme = &key
	ev.SetBannerImage(media.LinkedImageOf(media.Media{Key: "images/afis.jpg", Kind: media.KindImage, Type: "image/jpeg", Visibility: media.VisibilityPublic, Width: 1200, Height: 400}))
	e.events[ev.ID], e.ev = ev, ev
	e.member(user.AccountActive)
	if r := e.pass(t); r.Sent != 1 || len(e.mail.calls) != 1 {
		t.Fatalf("report %+v", r)
	}
	vars := e.mail.calls[0].vars
	if vars["Theme"] != "artlab" || !strings.HasPrefix(vars["EventBannerUrl"], "https://") || !strings.HasSuffix(vars["EventBannerUrl"], "afis.jpg") {
		t.Fatalf("vars %v", vars)
	}
}
