package event_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

// An Event's theme and ticket colour (docs/event-theme.md).

func TestThemeAndTicketColourRules(t *testing.T) {
	t.Parallel()
	str := func(s string) *string { return &s }
	for _, tc := range []struct {
		raw  *string
		want string
	}{{nil, ""}, {str(""), ""}, {str(" kurumsal "), ""}, {str("artlab"), "artlab"}, {str(" artlab "), "artlab"}} {
		got, err := event.NormalizeTheme(tc.raw)
		if err != nil || (got == nil) != (tc.want == "") || (got != nil && *got != tc.want) {
			t.Errorf("%v: %v %v", tc.raw, got, err)
		}
	}
	for _, raw := range []string{"ARTLAB", "bilinmeyen", "art lab", strings.Repeat("a", 33)} {
		if _, err := event.NormalizeTheme(&raw); !errors.Is(err, event.ErrThemeUnknown) || !errors.Is(err, event.ErrInvalid) {
			t.Errorf("%q: %v", raw, err)
		}
	}
	for raw, want := range map[string]string{"": "", " #ff8a1f ": "#FF8A1F", "#00AAff": "#00AAFF"} {
		got, err := event.NormalizeTicketAccentColor(&raw)
		if err != nil || (got == nil) != (want == "") || (got != nil && *got != want) {
			t.Errorf("%q: %v %v", raw, got, err)
		}
	}
	for _, raw := range []string{"ff8a1f", "#ff8a1", "#ff8a1fa", "#gg8a1f", "red", "rgb(1,2,3)"} {
		if _, err := event.NormalizeTicketAccentColor(&raw); !errors.Is(err, event.ErrTicketAccentColorInvalid) || !errors.Is(err, event.ErrInvalid) {
			t.Errorf("%q: %v", raw, err)
		}
	}
}

// Create and the update keep both, an update that does not send them keeps
// them, null (and "kurumsal" for the theme) clears them, anybody who reads
// the Event sees them, and a copy carries them.
func TestThemeOnCreatePatchAndCopy(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := event.NewService(event.NewMemoryStore(), authz.NewAuthorizer(authz.DefaultPolicy()))
	lead := authz.Principal{ID: "lider", Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
	bad := "neon"
	if _, err := svc.Create(ctx, lead, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB", Theme: &bad}); !errors.Is(err, event.ErrThemeUnknown) {
		t.Fatalf("unknown theme on create: %v", err)
	}
	key, color := "artlab", "#ff8a1f"
	created, err := svc.Create(ctx, lead, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB", Theme: &key, TicketAccentColor: &color})
	if err != nil || created.Theme == nil || *created.Theme != "artlab" || created.TicketAccentColor == nil || *created.TicketAccentColor != "#FF8A1F" {
		t.Fatalf("%v %v %v", created.Theme, created.TicketAccentColor, err)
	}
	patch := func(body string) (event.Event, error) {
		var p event.Patch
		if err := json.Unmarshal([]byte(body), &p); err != nil {
			t.Fatal(err)
		}
		return svc.Patch(ctx, lead, created.ID, p)
	}
	kept, err := patch(`{"name":"Hack 2"}`)
	if err != nil || kept.ThemeKey() != "artlab" || *kept.TicketAccentColor != "#FF8A1F" {
		t.Fatalf("kept %v %v", kept.Theme, err)
	}
	if _, err := patch(`{"theme":"ARTLAB"}`); !errors.Is(err, event.ErrThemeUnknown) {
		t.Fatalf("unknown theme: %v", err)
	}
	if _, err := patch(`{"ticketAccentColor":"turuncu"}`); !errors.Is(err, event.ErrTicketAccentColorInvalid) {
		t.Fatalf("bad colour: %v", err)
	}
	read, err := svc.Get(ctx, nil, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(read)
	if !strings.Contains(string(raw), `"theme":"artlab"`) || !strings.Contains(string(raw), `"ticketAccentColor":"#FF8A1F"`) {
		t.Fatalf("public read %s", raw)
	}
	start := time.Date(2027, 3, 14, 16, 0, 0, 0, time.UTC)
	copied, _, err := svc.Copy(ctx, lead, created.ID, event.CopyInput{ID: uuid.New(), Name: "Hack 2027", StartDate: &start})
	if err != nil || copied.Event.ThemeKey() != "artlab" || copied.Event.TicketAccentColor == nil || *copied.Event.TicketAccentColor != "#FF8A1F" {
		t.Fatalf("copy %v %v %v", copied.Event.Theme, copied.Event.TicketAccentColor, err)
	}
	for _, body := range []string{`{"theme":null,"ticketAccentColor":null}`, `{"theme":"kurumsal","ticketAccentColor":""}`, `{"theme":"","ticketAccentColor":null}`} {
		if _, err := patch(`{"theme":"artlab","ticketAccentColor":"#000000"}`); err != nil {
			t.Fatal(err)
		}
		cleared, err := patch(body)
		if err != nil || cleared.Theme != nil || cleared.TicketAccentColor != nil || cleared.ThemeKey() != "kurumsal" {
			t.Fatalf("%s: %v %v %v", body, cleared.Theme, cleared.TicketAccentColor, err)
		}
		raw, _ := json.Marshal(cleared)
		if !strings.Contains(string(raw), `"theme":null`) || !strings.Contains(string(raw), `"ticketAccentColor":null`) {
			t.Fatalf("Kurumsal answers null: %s", raw)
		}
	}
}

// Every participant's mail carries Theme ("" for Kurumsal, and for a key
// the catalogue lost) and EventBannerUrl: the poster's page size, an https
// JPEG or PNG, else "".
func TestMailLook(t *testing.T) {
	t.Parallel()
	if got := event.MailLook(event.Event{}); len(got) != 2 || got["Theme"] != "" || got["EventBannerUrl"] != "" {
		t.Fatalf("Kurumsal: %v", got)
	}
	key, lost := "artlab", "artlab-2019"
	if got := event.MailLook(event.Event{Theme: &key}); got["Theme"] != "artlab" {
		t.Fatalf("artlab: %v", got)
	}
	if got := event.MailLook(event.Event{Theme: &lost}); got["Theme"] != "" {
		t.Fatalf("a lost key: %v", got)
	}
	vars := event.WithMailLook(map[string]string{"EventName": "ARTLAB"}, event.Event{Theme: &key})
	if len(vars) != 3 || vars["Theme"] != "artlab" || vars["EventBannerUrl"] != "" {
		t.Fatalf("%v", vars)
	}
	for _, tc := range []struct {
		m    media.Media
		want string
	}{
		{media.Media{Key: "images/afis.jpg", Kind: media.KindImage, Type: "image/jpeg", Visibility: media.VisibilityPublic, Width: 900, Height: 300}, "/images/afis.jpg"},
		{media.Media{Key: "images/afis.webp", Kind: media.KindImage, Type: "image/webp", Visibility: media.VisibilityPublic}, ""},
		{media.Media{Key: "images/gizli.png", Kind: media.KindImage, Type: "image/png", Visibility: media.VisibilityPrivate}, ""},
	} {
		var ev event.Event
		ev.SetBannerImage(media.LinkedImageOf(tc.m))
		got := ev.BannerURL()
		if (tc.want == "") != (got == "") || !strings.HasSuffix(got, tc.want) || (got != "" && !strings.HasPrefix(got, "https://")) {
			t.Errorf("%s: %q", tc.m.Key, got)
		}
	}
}
