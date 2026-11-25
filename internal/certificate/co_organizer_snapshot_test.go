package certificate_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/certificate"
	"github.com/skylab-kulubu/core-backend/internal/event"
)

// A certificate keeps the Event's Co-organizer teams shown outside the
// panel when it was issued (docs/co-organizers.md): its verification names
// them, never a helper team; a later change of the list leaves it as it
// was. An Event without them answers no such field.
func TestCertificateCoOrganizerSnapshot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newProgramFixture(t, eightTalks())
	ayse := f.member(t, "Ayşe", "Yılmaz", "Talk 1", "Talk 2", "Talk 3", "Talk 4", "Talk 5", "Talk 6")
	if _, err := f.events.SetCoOrganizers(ctx, event.CoOrganizersWrite{
		EventID: f.ev.ID, OwnerTeam: f.ev.OwnerTeam,
		Teams: []event.CoOrganizerTeam{
			{Code: "WEBLAB", Management: event.ManagementLeaders, Visibility: event.VisibilityCoOrganizer},
			{Code: "GECEKODU", Management: event.ManagementMembers, Visibility: event.VisibilityHelper},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Finalize(ctx, f.leader, f.ev.ID); err != nil {
		t.Fatal(err)
	}
	processAll(t, f)
	issued, _ := certificatesOf(t, f, ayse)
	if len(issued.CoOrganizerTeams) != 1 || issued.CoOrganizerTeams[0] != "WEBLAB" {
		t.Fatalf("snapshot %v", issued.CoOrganizerTeams)
	}
	// The list changes: the certificate does not.
	if _, err := f.events.SetCoOrganizers(ctx, event.CoOrganizersWrite{EventID: f.ev.ID, OwnerTeam: f.ev.OwnerTeam}); err != nil {
		t.Fatal(err)
	}
	public, err := f.svc.Verify(ctx, issued.Serial)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(public)
	if public.CoOrganizerNames != "WEBLAB" || strings.Contains(string(raw), "GECEKODU") {
		t.Fatalf("verification %s", raw)
	}
}

// Without Co-organizers the verification answers no coOrganizerNames.
func TestCertificateWithoutCoOrganizers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newProgramFixture(t, eightTalks())
	ayse := f.member(t, "Ayşe", "Yılmaz", "Talk 1", "Talk 2", "Talk 3", "Talk 4", "Talk 5", "Talk 6")
	if _, err := f.svc.Finalize(ctx, f.leader, f.ev.ID); err != nil {
		t.Fatal(err)
	}
	processAll(t, f)
	issued, _ := certificatesOf(t, f, ayse)
	if issued.CoOrganizerTeams == nil || len(issued.CoOrganizerTeams) != 0 {
		t.Fatalf("snapshot %#v: want an empty list, not none", issued.CoOrganizerTeams)
	}
	public, err := f.svc.Verify(ctx, issued.Serial)
	if err != nil {
		t.Fatal(err)
	}
	if raw, _ := json.Marshal(public); strings.Contains(string(raw), "coOrganizerNames") {
		t.Fatalf("verification %s", raw)
	}
}

// An Event's binding takes a template of one of its Co-organizer teams,
// for who may read that team's templates; another team's stays refused
// (400, as before).
func TestEventBindingTakesACoOrganizerTeamsTemplate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAttendanceFixture(t, certificate.RuleCount)
	if _, err := f.events.SetCoOrganizers(ctx, event.CoOrganizersWrite{
		EventID: f.ev.ID, OwnerTeam: f.ev.OwnerTeam,
		Teams: []event.CoOrganizerTeam{{Code: "WEBLAB", Management: event.ManagementLeaders, Visibility: event.VisibilityHelper}},
	}); err != nil {
		t.Fatal(err)
	}
	template := func(team string) uuid.UUID {
		t.Helper()
		item, err := f.store.CreateTemplate(ctx, certificate.Template{ID: uuid.New(), Name: team, OwnerTeam: team, SourceKind: "figma", DraftLayout: versionedLayout("V1")})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.store.CreateVersion(ctx, certificate.TemplateVersion{ID: uuid.New(), TemplateID: item.ID, Layout: item.DraftLayout, Checksum: team}); err != nil {
			t.Fatal(err)
		}
		return item.ID
	}
	weblab, algolab := template("WEBLAB"), template("ALGOLAB")
	bind := func(p authz.Principal, id uuid.UUID) error {
		_, err := f.svc.SetBinding(ctx, p, certificate.Binding{Scope: certificate.ScopeEvent, ScopeKey: f.ev.ID.String(), TemplateID: id})
		return err
	}
	weblabLeader := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
	if err := bind(weblabLeader, weblab); err != nil {
		t.Fatalf("WEBLAB's Leader binds WEBLAB's template: %v", err)
	}
	if err := bind(f.leader, weblab); !errors.Is(err, certificate.ErrForbidden) {
		t.Fatalf("ARTLAB's Leader, who may not read WEBLAB's templates, binds one: %v", err)
	}
	if err := bind(f.leader, algolab); !errors.Is(err, certificate.ErrInvalid) {
		t.Fatalf("another team's template: %v", err)
	}
}
