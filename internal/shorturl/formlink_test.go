package shorturl

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
)

var formsService = authz.Principal{ID: "22222222-2222-2222-2222-222222222222", Roles: []string{"url:forms"}}

func formTarget(id uuid.UUID) string {
	return "https://forms.yildizskylab.com/" + id.String()
}

func formLinkService() Service {
	return NewService(NewMemoryStore(), authz.NewAuthorizer(authz.DefaultPolicy()))
}

func TestEnsureFormLinkKeepsOneLinkPerForm(t *testing.T) {
	t.Parallel()
	svc := formLinkService()
	ctx := context.Background()
	formID := uuid.New()
	actor := uuid.New()

	first, err := svc.EnsureFormLink(ctx, formsService, formID, FormLinkInput{URL: formTarget(formID), Label: "Yaz Kampı", ActorID: &actor})
	if err != nil {
		t.Fatal(err)
	}
	if first.FormID == nil || *first.FormID != formID || first.Label != "Yaz Kampı" || first.Source() != SourceForm {
		t.Fatalf("first link: %+v", first)
	}
	if first.CreatedBy == nil || *first.CreatedBy != actor {
		t.Fatalf("creator: %+v", first.CreatedBy)
	}

	again, err := svc.EnsureFormLink(ctx, formsService, formID, FormLinkInput{URL: formTarget(formID), Label: "Yaz Kampı 2026"})
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != first.ID || again.Alias != first.Alias || again.Label != "Yaz Kampı 2026" {
		t.Fatalf("second ensure: %+v", again)
	}

	member := authz.Principal{ID: uuid.NewString(), Roles: []string{"url:access"}}
	if _, err := svc.EnsureFormLink(ctx, member, formID, FormLinkInput{URL: formTarget(formID)}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("member ensure: %v", err)
	}
	if _, err := svc.EnsureFormLink(ctx, formsService, uuid.New(), FormLinkInput{URL: formTarget(formID)}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("target of another form: %v", err)
	}
}

func TestEnsureFormLinkUsesTheReadableSuggestion(t *testing.T) {
	t.Parallel()
	svc := formLinkService()
	ctx := context.Background()
	first, second, third := uuid.New(), uuid.New(), uuid.New()

	link, err := svc.EnsureFormLink(ctx, formsService, first, FormLinkInput{URL: formTarget(first), Alias: "Yaz-Kampi-2026"})
	if err != nil {
		t.Fatal(err)
	}
	if link.Alias != "yaz-kampi-2026" {
		t.Fatalf("suggested alias: %+v", link)
	}
	numbered, err := svc.EnsureFormLink(ctx, formsService, second, FormLinkInput{URL: formTarget(second), Alias: "yaz-kampi-2026"})
	if err != nil {
		t.Fatal(err)
	}
	if numbered.Alias != "yaz-kampi-2026-2" {
		t.Fatalf("taken suggestion is numbered: %+v", numbered)
	}
	random, err := svc.EnsureFormLink(ctx, formsService, third, FormLinkInput{URL: formTarget(third), Alias: "--"})
	if err != nil {
		t.Fatal(err)
	}
	if len(random.Alias) != 8 {
		t.Fatalf("an unusable suggestion falls back to a random alias: %+v", random)
	}
	if got := numberedAlias(strings.Repeat("a", 64), 3); got != strings.Repeat("a", 62)+"-3" {
		t.Fatalf("numbered alias keeps the 64 rune limit: %s", got)
	}
}

func TestRenameFormLinkKeepsThePreviousAliasPointingAtTheForm(t *testing.T) {
	t.Parallel()
	svc := formLinkService()
	ctx := context.Background()
	formID := uuid.New()
	owner := authz.Principal{ID: "11111111-1111-1111-1111-111111111111", Roles: []string{"url:access"}}

	link, err := svc.EnsureFormLink(ctx, formsService, formID, FormLinkInput{URL: formTarget(formID), Alias: "yaz-kampi-2026"})
	if err != nil {
		t.Fatal(err)
	}
	renamed, err := svc.RenameFormLink(ctx, formsService, formID, "yaz-kampi", "")
	if err != nil {
		t.Fatal(err)
	}
	if renamed.ID != link.ID || renamed.Alias != "yaz-kampi" {
		t.Fatalf("renamed: %+v", renamed)
	}
	old, err := svc.Redirect(ctx, link.Alias, Hit{UTM: UTM{Source: "qr"}})
	if err != nil {
		t.Fatalf("a printed QR code for the old alias must keep working: %v", err)
	}
	if old.ID != link.ID {
		t.Fatalf("old alias points elsewhere: %+v", old)
	}
	if _, err := svc.Create(ctx, owner, "https://example.com", link.Alias); !errors.Is(err, ErrConflict) {
		t.Fatalf("retired alias handed out again: %v", err)
	}
	if _, err := svc.Create(ctx, owner, "https://example.com", "YAZ-KAMPI"); !errors.Is(err, ErrConflict) {
		t.Fatalf("alias differing only in case: %v", err)
	}

	back, err := svc.RenameFormLink(ctx, formsService, formID, "", "yaz-kampi-2026")
	if err != nil {
		t.Fatalf("taking its own default back: %v", err)
	}
	if back.Alias != "yaz-kampi-2026" {
		t.Fatalf("back: %+v", back)
	}
	if again, err := svc.RenameFormLink(ctx, formsService, formID, "", "yaz-kampi-2026"); err != nil || again.Alias != "yaz-kampi-2026" {
		t.Fatalf("asking for the default it already has: %+v %v", again, err)
	}
	if _, err := svc.RenameFormLink(ctx, formsService, formID, "c", ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("reserved alias: %v", err)
	}
}

func TestRenameFormLinkRefusesAnUnusableSuggestion(t *testing.T) {
	t.Parallel()
	svc := formLinkService()
	ctx := context.Background()
	formID := uuid.New()

	link, err := svc.EnsureFormLink(ctx, formsService, formID, FormLinkInput{URL: formTarget(formID), Alias: "yaz-kampi-2026"})
	if err != nil {
		t.Fatal(err)
	}
	for _, suggestion := range []string{"", "--"} {
		if _, err := svc.RenameFormLink(ctx, formsService, formID, "", suggestion); !errors.Is(err, ErrInvalid) {
			t.Fatalf("suggestion %q gives no alias: %v", suggestion, err)
		}
	}
	current, err := svc.FormLink(ctx, formsService, formID)
	if err != nil || current.Alias != link.Alias {
		t.Fatalf("a refused rename keeps the alias: %+v %v", current, err)
	}
}

func TestRestoreFormLinkConflictsWhenTheFormHasAnother(t *testing.T) {
	t.Parallel()
	svc := formLinkService()
	ctx := context.Background()
	formID := uuid.New()
	moderator := authz.Principal{ID: "33333333-3333-3333-3333-333333333333", Roles: []string{"url:moderator"}}

	first, err := svc.EnsureFormLink(ctx, formsService, formID, FormLinkInput{URL: formTarget(formID)})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Delete(ctx, moderator, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.EnsureFormLink(ctx, formsService, formID, FormLinkInput{URL: formTarget(formID)}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Restore(ctx, moderator, first.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("restoring a replaced form link: %v", err)
	}
}

func TestSyncEventFormsTakesTheFormLinkOverAndHandsItBack(t *testing.T) {
	t.Parallel()
	svc := formLinkService()
	ctx := context.Background()
	formID := uuid.New()
	eventID := uuid.New()
	panel := authz.Principal{ID: "33333333-3333-3333-3333-333333333333", Roles: []string{"url:access"}}

	own, err := svc.EnsureFormLink(ctx, formsService, formID, FormLinkInput{URL: formTarget(formID)})
	if err != nil {
		t.Fatal(err)
	}
	eventLink, err := svc.Create(ctx, panel, formTarget(formID), "yazkampi")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.SyncEventForms(ctx, eventID, []EventForm{{FormID: formID, URL: formTarget(formID), Alias: "yazkampi", Label: "Yaz Kampı 2026"}}); err != nil {
		t.Fatal(err)
	}
	bound, err := svc.FormLink(ctx, formsService, formID)
	if err != nil {
		t.Fatal(err)
	}
	if bound.ID != eventLink.ID || bound.EventID == nil || *bound.EventID != eventID || bound.Source() != SourceEvent || bound.Label != "Yaz Kampı 2026" {
		t.Fatalf("bound: %+v", bound)
	}
	if _, err := svc.Lookup(ctx, own.Alias); err != nil {
		t.Fatalf("the form's earlier link must keep redirecting: %v", err)
	}
	if _, err := svc.RenameFormLink(ctx, formsService, formID, "baska-ad", ""); !errors.Is(err, ErrEventManaged) {
		t.Fatalf("form-side rename of an event link: %v", err)
	}
	if _, err := svc.Update(ctx, panel, eventLink.ID, "", "baska-ad"); !errors.Is(err, ErrManaged) {
		t.Fatalf("generic rename of a bound link: %v", err)
	}

	if err := svc.SyncEventForms(ctx, eventID, nil); err != nil {
		t.Fatal(err)
	}
	released, err := svc.FormLink(ctx, formsService, formID)
	if err != nil {
		t.Fatal(err)
	}
	if released.ID != eventLink.ID || released.EventID != nil || released.Source() != SourceForm {
		t.Fatalf("released: %+v", released)
	}

	feedback := uuid.New()
	if err := svc.SyncEventForms(ctx, eventID, []EventForm{{FormID: feedback, URL: formTarget(feedback), Alias: "geri-bildirim", Label: "Yaz Kampı 2026 · Geri bildirim"}}); err != nil {
		t.Fatal(err)
	}
	created, err := svc.FormLink(ctx, formsService, feedback)
	if err != nil {
		t.Fatal(err)
	}
	if created.Alias != "geri-bildirim" || created.EventID == nil || *created.EventID != eventID {
		t.Fatalf("created for the event: %+v", created)
	}

	if _, err := svc.Create(ctx, panel, "https://example.com", "baskasi"); err != nil {
		t.Fatal(err)
	}
	err = svc.SyncEventForms(ctx, eventID, []EventForm{{FormID: feedback, URL: formTarget(feedback), Alias: "baskasi"}})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("alias pointing elsewhere: %v", err)
	}
	stranger, err := svc.Lookup(ctx, "baskasi")
	if err != nil {
		t.Fatal(err)
	}
	if stranger.FormID != nil {
		t.Fatalf("stranger link was taken over: %+v", stranger)
	}
}

func TestAvailabilityExplainsARefusedAlias(t *testing.T) {
	t.Parallel()
	svc := formLinkService()
	ctx := context.Background()
	owner := authz.Principal{ID: "11111111-1111-1111-1111-111111111111", Roles: []string{"url:access"}}
	if _, err := svc.Create(ctx, owner, "https://example.com", "gecekodu"); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		alias     string
		available bool
		reason    string
	}{
		{"gecekodu", false, ReasonTaken},
		{"GeceKodu", false, ReasonTaken},
		{"c", false, ReasonReserved},
		{"api", false, ReasonReserved},
		{"-yaz", false, ReasonInvalid},
		{"yaz kampi", false, ReasonInvalid},
		{"yaz-kampi", true, ""},
	}
	for _, tc := range cases {
		got, err := svc.Availability(ctx, formsService, tc.alias)
		if err != nil {
			t.Fatalf("%s: %v", tc.alias, err)
		}
		if got.Available != tc.available || got.Reason != tc.reason {
			t.Fatalf("%s: %+v", tc.alias, got)
		}
	}
	if _, err := svc.Availability(ctx, authz.Principal{ID: uuid.NewString()}, "yaz-kampi"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("anonymous-ish caller: %v", err)
	}
}

func TestFormStatsCountsEveryLinkToTheForm(t *testing.T) {
	t.Parallel()
	svc := formLinkService()
	ctx := context.Background()
	formID := uuid.New()
	owner := authz.Principal{ID: "11111111-1111-1111-1111-111111111111", Roles: []string{"url:access"}}

	link, err := svc.EnsureFormLink(ctx, formsService, formID, FormLinkInput{URL: formTarget(formID)})
	if err != nil {
		t.Fatal(err)
	}
	personal, err := svc.Create(ctx, owner, formTarget(formID), "kisisel")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(ctx, owner, "https://example.com", "baska"); err != nil {
		t.Fatal(err)
	}
	for _, hop := range []struct {
		alias  string
		source string
	}{
		{link.Alias, "instagram"},
		{link.Alias, "instagram"},
		{link.Alias, "qr"},
		{personal.Alias, ""},
		{"baska", "instagram"},
	} {
		if _, err := svc.Redirect(ctx, hop.alias, Hit{UTM: UTM{Source: hop.source}}); err != nil {
			t.Fatal(err)
		}
	}

	stats, err := svc.FormStats(ctx, formsService, formID)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"instagram": 2, "qr": 1, "": 1}
	if stats.Total != 4 || len(stats.Sources) != len(want) {
		t.Fatalf("stats: %+v", stats)
	}
	for _, c := range stats.Sources {
		if want[c.Source] != c.Count {
			t.Fatalf("source %q: %d", c.Source, c.Count)
		}
	}
}
