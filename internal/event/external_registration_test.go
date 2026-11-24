package event_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
)

// An Event's external registration link (docs/attendee-import.md).

func TestExternalRegistrationURLRule(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]string{
		"":                                    "",
		"  ":                                  "",
		" https://forms.office.com/r/AbC123 ": "https://forms.office.com/r/AbC123",
		"HTTPS://forms.office.com/Pages/x?id=1&lang=tr": "https://forms.office.com/Pages/x?id=1&lang=tr",
		"https://lu.ma/etkinlik":                        "https://lu.ma/etkinlik",
	} {
		got, err := event.NormalizeExternalRegistrationURL(raw)
		if err != nil || got != want {
			t.Errorf("%q: %q %v", raw, got, err)
		}
	}
	for _, raw := range []string{
		"http://forms.office.com/r/x",
		"forms.office.com/r/x",
		"javascript:alert(1)",
		"https://",
		"https:///path",
		"https://user:pass@forms.office.com/r/x",
		"https://forms.office.com/r/a b",
		"https://forms.office.com/\nx",
		"ftp://example.com",
		"https://example.com/" + strings.Repeat("a", event.MaxExternalRegistrationURL),
	} {
		if _, err := event.NormalizeExternalRegistrationURL(raw); !errors.Is(err, event.ErrExternalRegistrationURLInvalid) || !errors.Is(err, event.ErrInvalid) {
			t.Errorf("%q: %v", raw, err)
		}
	}
}

// Create and the update keep the link, an update that does not send it
// keeps it, "" and null clear it, and anybody who reads the Event sees it.
func TestExternalRegistrationURLOnCreateAndPatch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := event.NewService(event.NewMemoryStore(), authz.NewAuthorizer(authz.DefaultPolicy()))
	lead := authz.Principal{ID: "lider", Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
	if _, err := svc.Create(ctx, lead, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB", ExternalRegistrationURL: "http://x.example.com"}); !errors.Is(err, event.ErrExternalRegistrationURLInvalid) {
		t.Fatalf("http on create: %v", err)
	}
	created, err := svc.Create(ctx, lead, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB", ExternalRegistrationURL: " https://forms.office.com/r/AbC "})
	if err != nil || created.ExternalRegistrationURL != "https://forms.office.com/r/AbC" {
		t.Fatalf("%q %v", created.ExternalRegistrationURL, err)
	}
	patch := func(body string) (event.Event, error) {
		var p event.Patch
		if err := json.Unmarshal([]byte(body), &p); err != nil {
			t.Fatal(err)
		}
		return svc.Patch(ctx, lead, created.ID, p)
	}
	kept, err := patch(`{"name":"Hack 2"}`)
	if err != nil || kept.ExternalRegistrationURL != "https://forms.office.com/r/AbC" {
		t.Fatalf("kept %q %v", kept.ExternalRegistrationURL, err)
	}
	if _, err := patch(`{"externalRegistrationUrl":"https://user:x@evil.example.com"}`); !errors.Is(err, event.ErrExternalRegistrationURLInvalid) {
		t.Fatalf("userinfo: %v", err)
	}
	read, err := svc.Get(ctx, nil, created.ID)
	if err != nil || read.ExternalRegistrationURL != "https://forms.office.com/r/AbC" {
		t.Fatalf("public read %q %v", read.ExternalRegistrationURL, err)
	}
	raw, _ := json.Marshal(read)
	if !strings.Contains(string(raw), `"externalRegistrationUrl":"https://forms.office.com/r/AbC"`) {
		t.Fatalf("json %s", raw)
	}
	for _, body := range []string{`{"externalRegistrationUrl":""}`, `{"externalRegistrationUrl":null}`} {
		if _, err := patch(`{"externalRegistrationUrl":"https://lu.ma/x"}`); err != nil {
			t.Fatal(err)
		}
		cleared, err := patch(body)
		if err != nil || cleared.ExternalRegistrationURL != "" {
			t.Fatalf("%s: %q %v", body, cleared.ExternalRegistrationURL, err)
		}
		raw, _ := json.Marshal(cleared)
		if strings.Contains(string(raw), "externalRegistrationUrl") {
			t.Fatalf("an Event without a link leaves it out: %s", raw)
		}
	}
}
