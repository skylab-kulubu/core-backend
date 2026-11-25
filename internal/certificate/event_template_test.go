package certificate_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/certificate"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// eventTemplateFixture is a GECEKODU Event bound to nothing of its own: the
// GECEKODU team template serves it, over the club default. An ARTLAB Event
// and an ARTLAB team template sit beside it.
type eventTemplateFixture struct {
	svc      certificate.Service
	store    *certificate.MemoryStore
	gecekodu event.Event
	artlab   event.Event
	club     certificate.Template
	team     certificate.Template
	other    certificate.Template
}

var (
	gecekoduMember = authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ORGANIZASYON/GECEKODU"}}
	artlabLeader   = authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/ARTLAB/LIDERLER"}}
	artlabMember   = authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/ARTLAB"}}
	dkMember       = authz.Principal{ID: uuid.NewString(), Groups: []string{"/DK"}}
	ykMember       = authz.Principal{ID: uuid.NewString(), Groups: []string{"/YK"}}
	adminMember    = authz.Principal{ID: uuid.NewString(), Groups: []string{"/ADMIN"}}
)

func newEventTemplateFixture(t *testing.T) eventTemplateFixture {
	t.Helper()
	ctx := context.Background()
	events := event.NewMemoryStore()
	store := certificate.NewMemoryStore()
	svc := certificate.NewServiceWithOptions(store, ticket.NewMemoryStore(), events, user.NewMemoryStore(), authz.NewAuthorizer(authz.DefaultPolicy()), &recRender{}, &recMail{}, certificate.Options{
		PublicAPIOrigin: "https://api.example.test", VerifyOrigin: "https://skyl.app/c", Templates: store, Jobs: store,
	})
	f := eventTemplateFixture{svc: svc, store: store}
	var err error
	if f.gecekodu, err = events.Create(ctx, event.Event{Name: "Gece Kodu 2026", Location: "YTÜ", OwnerTeam: "GECEKODU"}); err != nil {
		t.Fatal(err)
	}
	if f.artlab, err = events.Create(ctx, event.Event{Name: "ARTLAB Sergi", Location: "YTÜ", OwnerTeam: "ARTLAB"}); err != nil {
		t.Fatal(err)
	}
	published := func(name, team, label string, system bool) certificate.Template {
		t.Helper()
		item, err := store.CreateTemplate(ctx, certificate.Template{ID: uuid.New(), Name: name, OwnerTeam: team, SourceKind: "figma", SourceEditURL: "https://figma.example/" + label, System: system, DraftLayout: versionedLayout(label)})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.CreateVersion(ctx, certificate.TemplateVersion{ID: uuid.New(), TemplateID: item.ID, Layout: item.DraftLayout, Checksum: label}); err != nil {
			t.Fatal(err)
		}
		return item
	}
	f.club = published("SKY LAB", "", "CLUB", true)
	f.team = published("GECEKODU", "GECEKODU", "TEAM", false)
	f.other = published("ARTLAB", "ARTLAB", "OTHER", false)
	if _, err := store.CreateTemplate(ctx, certificate.Template{ID: uuid.New(), Name: "GECEKODU taslak", OwnerTeam: "GECEKODU", SourceKind: "sky", DraftLayout: versionedLayout("DRAFT")}); err != nil {
		t.Fatal(err)
	}
	for _, b := range []certificate.Binding{
		{Scope: certificate.ScopeClub, ScopeKey: "SKY_LAB", TemplateID: f.club.ID},
		{Scope: certificate.ScopeOwnerTeam, ScopeKey: "GECEKODU", TemplateID: f.team.ID},
	} {
		if _, err := store.SetBinding(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func (f eventTemplateFixture) create(t *testing.T, p authz.Principal, from string) certificate.Template {
	t.Helper()
	item, err := f.svc.CreateEventTemplate(context.Background(), p, f.gecekodu.ID, certificate.EventTemplateInput{From: from})
	if err != nil {
		t.Fatalf("create from %q: %v", from, err)
	}
	return item
}

// Who may edit the Event decides its Event template (K4): a GECEKODU member
// without a certificate role creates, edits, publishes and binds; another
// team's Leader may not; DK reads and writes nothing; ADMIN and YK do
// everything. The team template stays the team's.
func TestEventTemplateRules(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name                       string
		p                          authz.Principal
		read, write, bindTeam, own bool
	}{
		{"GECEKODU member", gecekoduMember, true, true, true, false},
		{"ARTLAB Leader", artlabLeader, false, false, false, false},
		{"ARTLAB member", artlabMember, false, false, false, false},
		{"DK", dkMember, true, false, false, false},
		{"YK", ykMember, true, true, true, true},
		{"ADMIN", adminMember, true, true, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newEventTemplateFixture(t)
			allowed := func(err error) bool {
				t.Helper()
				if err != nil && !errors.Is(err, certificate.ErrForbidden) {
					t.Fatalf("not a refusal: %v", err)
				}
				return err == nil
			}
			_, err := f.svc.EventTemplate(ctx, c.p, f.gecekodu.ID)
			if got := allowed(err); got != c.read {
				t.Errorf("state read %v, want %v", got, c.read)
			}
			_, err = f.svc.CreateEventTemplate(ctx, c.p, f.gecekodu.ID, certificate.EventTemplateInput{From: "current"})
			if got := allowed(err); got != c.write {
				t.Errorf("create %v, want %v", got, c.write)
			}
			// The rest on a template ADMIN made.
			own, err := f.svc.ListTemplates(ctx, adminMember, certificate.TemplateFilter{EventID: &f.gecekodu.ID})
			if err != nil {
				t.Fatal(err)
			}
			var item certificate.Template
			if len(own) == 1 {
				item = own[0]
			} else {
				item = f.create(t, adminMember, "current")
			}
			_, err = f.svc.GetTemplate(ctx, c.p, item.ID)
			if got := allowed(err); got != c.read {
				t.Errorf("read %v, want %v", got, c.read)
			}
			_, err = f.svc.UpdateTemplate(ctx, c.p, item.ID, certificate.TemplateDraft{Name: "Yeni ad", SourceKind: "sky", Layout: versionedLayout("EDITED")})
			if got := allowed(err); got != c.write {
				t.Errorf("update %v, want %v", got, c.write)
			}
			_, err = f.svc.PublishTemplate(ctx, c.p, item.ID)
			if got := allowed(err); got != c.write {
				t.Errorf("publish %v, want %v", got, c.write)
			}
			_, err = f.svc.SetBinding(ctx, c.p, certificate.Binding{Scope: certificate.ScopeEvent, ScopeKey: f.gecekodu.ID.String(), TemplateID: f.team.ID})
			if got := allowed(err); got != c.bindTeam {
				t.Errorf("bind to the team template %v, want %v", got, c.bindTeam)
			}
			err = f.svc.ClearBinding(ctx, c.p, certificate.ScopeEvent, f.gecekodu.ID.String())
			if err != nil && errors.Is(err, certificate.ErrNotFound) {
				err = nil // nothing left to clear
			}
			if got := allowed(err); got != c.bindTeam {
				t.Errorf("clear %v, want %v", got, c.bindTeam)
			}
			_, err = f.svc.UpdateTemplate(ctx, c.p, f.team.ID, certificate.TemplateDraft{Name: "GECEKODU", OwnerTeam: "GECEKODU", SourceKind: "figma", Layout: versionedLayout("TEAM 2")})
			if got := allowed(err); got != c.own {
				t.Errorf("edit the team template %v, want %v", got, c.own)
			}
			// Another team's Event: the GECEKODU member may not.
			_, err = f.svc.CreateEventTemplate(ctx, gecekoduMember, f.artlab.ID, certificate.EventTemplateInput{From: "blank"})
			if !errors.Is(err, certificate.ErrForbidden) {
				t.Errorf("GECEKODU member on ARTLAB's Event: %v", err)
			}
		})
	}
}

// from: the template in use now (its published layout, its source), an
// empty page, or a published team or club template the Event may use.
func TestCreateEventTemplateFrom(t *testing.T) {
	ctx := context.Background()
	f := newEventTemplateFixture(t)
	current := f.create(t, gecekoduMember, "current")
	if current.EventID == nil || *current.EventID != f.gecekodu.ID || current.OwnerTeam != "GECEKODU" {
		t.Fatalf("owner %v %q", current.EventID, current.OwnerTeam)
	}
	// The member does not read the team template: its layout, not the link
	// to the team's design.
	if current.Name != "Gece Kodu 2026 sertifikası" || current.SourceKind != "figma" || current.SourceEditURL != "" || current.SourceRef != "" {
		t.Fatalf("name %q source %q %q %q", current.Name, current.SourceKind, current.SourceEditURL, current.SourceRef)
	}
	// Who reads it (ADMIN) gets the link too.
	if linked := newEventTemplateFixture(t).create(t, adminMember, "current"); linked.SourceKind != "figma" || linked.SourceEditURL != "https://figma.example/TEAM" {
		t.Fatalf("reader's copy %q %q", linked.SourceKind, linked.SourceEditURL)
	}
	if !reflect.DeepEqual(current.DraftLayout, versionedLayout("TEAM")) || current.PublishedVersion != nil {
		t.Fatalf("layout %+v published %v", current.DraftLayout, current.PublishedVersion)
	}
	if current.CreatedBy == nil || current.CreatedBy.String() != gecekoduMember.ID {
		t.Fatalf("created by %v", current.CreatedBy)
	}

	// A second one: 409 with the current one's id.
	_, err := f.svc.CreateEventTemplate(ctx, gecekoduMember, f.gecekodu.ID, certificate.EventTemplateInput{From: "blank"})
	var exists *certificate.EventTemplateExistsError
	if !errors.As(err, &exists) || exists.TemplateID != current.ID || !errors.Is(err, certificate.ErrConflict) {
		t.Fatalf("second: %v", err)
	}

	f = newEventTemplateFixture(t)
	blank := f.create(t, gecekoduMember, "blank")
	if blank.SourceKind != "sky" || len(blank.DraftLayout.Elements) != 3 {
		t.Fatalf("blank %q %d", blank.SourceKind, len(blank.DraftLayout.Elements))
	}
	if err := certificate.ValidateLayout(blank.DraftLayout); err != nil {
		t.Fatalf("blank layout: %v", err)
	}
	// The blank one saves and publishes as it is.
	if _, err := f.svc.PublishTemplate(ctx, gecekoduMember, blank.ID); err != nil {
		t.Fatalf("publish blank: %v", err)
	}

	f = newEventTemplateFixture(t)
	club := f.create(t, gecekoduMember, f.club.ID.String())
	if !reflect.DeepEqual(club.DraftLayout, versionedLayout("CLUB")) {
		t.Fatal("not the club template's layout")
	}
	// Only a published team or club template the Event may use: not
	// another team's, not another Event's Event template. None is created.
	g := newEventTemplateFixture(t)
	artlabOwn, err := g.svc.CreateEventTemplate(ctx, adminMember, g.artlab.ID, certificate.EventTemplateInput{From: "blank"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.svc.PublishTemplate(ctx, adminMember, artlabOwn.ID); err != nil {
		t.Fatal(err)
	}
	for from, want := range map[string]error{
		g.other.ID.String():   certificate.ErrNotFound,
		artlabOwn.ID.String(): certificate.ErrNotFound,
		uuid.NewString():      certificate.ErrNotFound,
		"not-an-id":           certificate.ErrInvalid,
	} {
		if _, err := g.svc.CreateEventTemplate(ctx, gecekoduMember, g.gecekodu.ID, certificate.EventTemplateInput{From: from}); !errors.Is(err, want) {
			t.Errorf("from %s: %v, want %v", from, err, want)
		}
	}
}

// Publishing an Event template binds its Event; until then the team
// template serves it (a draft is never used). Publishing again keeps the
// binding.
func TestPublishEventTemplateBindsItsEvent(t *testing.T) {
	ctx := context.Background()
	f := newEventTemplateFixture(t)
	item := f.create(t, gecekoduMember, "current")
	state, err := f.svc.EventTemplate(ctx, gecekoduMember, f.gecekodu.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state.Resolution == nil || state.Resolution.Template.ID != f.team.ID || state.Resolution.Source != certificate.SourceOwnerTeam {
		t.Fatalf("before publish: %+v", state.Resolution)
	}
	if state.EventTemplate == nil || state.EventTemplate.ID != item.ID || state.EventTemplate.PublishedVersion != nil {
		t.Fatalf("draft: %+v", state.EventTemplate)
	}
	// The member reads the team template's name, not its layout.
	if len(state.Resolution.Version.Layout.Elements) != 0 {
		t.Fatal("team template's layout shown to a member without its rule")
	}
	for i := 1; i <= 2; i++ {
		version, err := f.svc.PublishTemplate(ctx, gecekoduMember, item.ID)
		if err != nil || version.Version != i {
			t.Fatalf("publish %d: %d %v", i, version.Version, err)
		}
		resolved, err := f.svc.ResolveTemplate(ctx, gecekoduMember, f.gecekodu.ID)
		if err != nil || resolved.Template.ID != item.ID || resolved.Source != certificate.SourceEvent || resolved.Version.Version != i {
			t.Fatalf("after publish %d: %+v %v", i, resolved, err)
		}
		// Its own template in full to who edits the Event.
		if len(resolved.Version.Layout.Elements) == 0 {
			t.Fatal("event template's layout hidden from its editor")
		}
	}
	state, err = f.svc.EventTemplate(ctx, gecekoduMember, f.gecekodu.ID)
	if err != nil || state.EventTemplate.PublishedVersion == nil || *state.EventTemplate.PublishedVersion != 2 {
		t.Fatalf("published: %+v %v", state.EventTemplate, err)
	}
	// Back to the team template: clear the binding; the Event template
	// stays a template of the Event.
	if err := f.svc.ClearBinding(ctx, gecekoduMember, certificate.ScopeEvent, f.gecekodu.ID.String()); err != nil {
		t.Fatal(err)
	}
	if resolved, err := f.svc.ResolveTemplate(ctx, gecekoduMember, f.gecekodu.ID); err != nil || resolved.Template.ID != f.team.ID {
		t.Fatalf("cleared: %+v %v", resolved, err)
	}
	// Binding it back by hand is the member's too.
	if _, err := f.svc.SetBinding(ctx, gecekoduMember, certificate.Binding{Scope: certificate.ScopeEvent, ScopeKey: f.gecekodu.ID.String(), TemplateID: item.ID}); err != nil {
		t.Fatal(err)
	}
}

// An Event template serves its own Event only: another Event, a team or the
// club is template_event_scoped.
func TestEventTemplateBindsItsOwnEventOnly(t *testing.T) {
	ctx := context.Background()
	f := newEventTemplateFixture(t)
	item := f.create(t, adminMember, "current")
	if _, err := f.svc.PublishTemplate(ctx, adminMember, item.ID); err != nil {
		t.Fatal(err)
	}
	for _, b := range []certificate.Binding{
		{Scope: certificate.ScopeEvent, ScopeKey: f.artlab.ID.String(), TemplateID: item.ID},
		{Scope: certificate.ScopeOwnerTeam, ScopeKey: "GECEKODU", TemplateID: item.ID},
		{Scope: certificate.ScopeClub, ScopeKey: "SKY_LAB", TemplateID: item.ID},
	} {
		_, err := f.svc.SetBinding(ctx, adminMember, b)
		var refused *certificate.InvalidError
		if !errors.As(err, &refused) || refused.Code != certificate.CodeTemplateEventScoped {
			t.Errorf("%s %s: %v", b.Scope, b.ScopeKey, err)
		}
	}
}

// The template list leaves Event templates out; ?eventId= gives the
// Event's alone. The state's choices are the published team and club
// templates the Event may use, never their layouts.
func TestEventTemplateListsAndChoices(t *testing.T) {
	ctx := context.Background()
	f := newEventTemplateFixture(t)
	item := f.create(t, gecekoduMember, "current")
	all, err := f.svc.ListTemplates(ctx, adminMember, certificate.TemplateFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, listed := range all {
		if listed.ID == item.ID {
			t.Fatal("Event template in the template list")
		}
	}
	if len(all) != 4 {
		t.Fatalf("%d templates", len(all))
	}
	own, err := f.svc.ListTemplates(ctx, gecekoduMember, certificate.TemplateFilter{EventID: &f.gecekodu.ID})
	if err != nil || len(own) != 1 || own[0].ID != item.ID {
		t.Fatalf("event's: %v %v", own, err)
	}
	if _, err := f.svc.ListTemplates(ctx, artlabLeader, certificate.TemplateFilter{EventID: &f.gecekodu.ID}); !errors.Is(err, certificate.ErrForbidden) {
		t.Fatalf("ARTLAB Leader: %v", err)
	}
	state, err := f.svc.EventTemplate(ctx, gecekoduMember, f.gecekodu.ID)
	if err != nil {
		t.Fatal(err)
	}
	got := map[uuid.UUID]certificate.TemplateChoice{}
	for _, choice := range state.Choices {
		got[choice.ID] = choice
	}
	if len(got) != 2 || got[f.club.ID].Version != 1 || !got[f.club.ID].System || got[f.team.ID].OwnerTeam != "GECEKODU" {
		t.Fatalf("choices %+v", state.Choices)
	}
}

// Editing an Event template keeps its Event's Owner team, whatever the body
// says.
func TestEventTemplateKeepsItsOwnerTeam(t *testing.T) {
	ctx := context.Background()
	f := newEventTemplateFixture(t)
	item := f.create(t, gecekoduMember, "current")
	updated, err := f.svc.UpdateTemplate(ctx, gecekoduMember, item.ID, certificate.TemplateDraft{
		Name: "Gece Kodu özel", OwnerTeam: "ARTLAB", SourceKind: "sky", Layout: versionedLayout("EDITED"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.OwnerTeam != "GECEKODU" || updated.EventID == nil || *updated.EventID != f.gecekodu.ID || updated.Name != "Gece Kodu özel" {
		t.Fatalf("updated %q %v %q", updated.OwnerTeam, updated.EventID, updated.Name)
	}
}
