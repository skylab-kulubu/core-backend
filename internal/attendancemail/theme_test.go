package attendancemail_test

import (
	"context"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/mail"
)

// The door's mails carry the Event's theme and poster, always: Theme
// "artlab" for an ARTLAB Event, "" for Kurumsal (docs/event-theme.md#mails).
func TestDoorMailsCarryTheEventsTheme(t *testing.T) {
	ctx := context.Background()
	for _, key := range []string{"artlab", ""} {
		e := newEnv(t, event.RuleCount)
		ev := e.ev
		if key != "" {
			ev.Theme = &key
		}
		if _, err := e.events.Update(ctx, ev); err != nil {
			t.Fatal(err)
		}
		ids, _ := e.program(2)
		e.welcome(e.member)
		e.recorded(e.guest, ids[0], e.now.Add(-3*60e9))
		if _, err := e.svc.Pass(ctx); err != nil {
			t.Fatal(err)
		}
		if len(e.mail.sent) == 0 {
			t.Fatal("nothing sent")
		}
		kinds := map[string]bool{}
		for _, got := range e.mail.sent {
			kinds[got.typ] = true
			theme, ok := got.vars["Theme"]
			banner, hasBanner := got.vars["EventBannerUrl"]
			if !ok || !hasBanner || theme != key || banner != "" {
				t.Errorf("%q %s: Theme %v (%v), EventBannerUrl %v (%v)", key, got.typ, theme, ok, banner, hasBanner)
			}
		}
		if !kinds[mail.AttendanceWelcomeType] {
			t.Errorf("%q: no welcome among %v", key, kinds)
		}
	}
}
