package event_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

// sourceEventTemplate makes d.template the source's published Event
// template, bound to it, with a Figma link: its draft and versions 1 and 2.
func sourceEventTemplate(t *testing.T, d copyDatabase) {
	t.Helper()
	ctx := context.Background()
	for _, q := range []string{
		`UPDATE certificate_templates SET event_id = $2, owner_team = 'WEBLAB', source_kind = 'figma',
			source_ref = 'ref-1', source_edit_url = 'https://figma.example/design', draft_layout = '{"width": 1123, "label": "draft"}'
		 WHERE id = $1`,
		`INSERT INTO certificate_template_versions (id, template_id, version, layout, asset_manifest, checksum, asset_serving_policy_applied)
		 VALUES (gen_random_uuid(), $1, 1, '{"label": "v1"}', '{}', 'sum-1', false)`,
		`INSERT INTO certificate_template_versions (id, template_id, version, layout, asset_manifest, checksum, asset_serving_policy_applied)
		 VALUES (gen_random_uuid(), $1, 2, '{"label": "v2"}', '{"k": {"key": "certificate-template-assets/v2/k", "contentType": "image/png"}}', 'sum-2', true)`,
	} {
		args := []any{d.template}
		if strings.Contains(q, "$2") {
			args = append(args, d.source.ID)
		}
		if _, err := d.pool.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
}

// copiedTemplate is the copy's current Event template: its id, name, Owner
// team, links and draft; ok false when it has none.
type copiedTemplate struct {
	id                    uuid.UUID
	name, team, ref, link string
	draft                 string
}

func copyTemplateOf(t *testing.T, d copyDatabase, copyID uuid.UUID) (copiedTemplate, bool) {
	t.Helper()
	var c copiedTemplate
	err := d.pool.QueryRow(context.Background(), `
		SELECT id, name, owner_team, source_ref, source_edit_url, draft_layout::text
		FROM certificate_templates WHERE event_id = $1 AND archived_at IS NULL`, copyID).
		Scan(&c.id, &c.name, &c.team, &c.ref, &c.link, &c.draft)
	if err != nil {
		return copiedTemplate{}, false
	}
	return c, true
}

// A copy of an Event with an Event template gets its own (K5): a new
// template of the copy with the same draft, the latest version as version 1
// (the same layout, asset copies and checksum), bound to the copy. Editing
// it leaves the source's alone.
func TestPostgresCopyEventTemplate(t *testing.T) {
	d := newCopyDatabase(t)
	ctx := context.Background()
	sourceEventTemplate(t, d)
	got, _, err := d.svc.Copy(ctx, d.yk, d.source.ID, copyIn("Gecekodu 2027", "2027-03-13T19:00:00+03:00"))
	if err != nil || len(got.Skipped) != 0 {
		t.Fatalf("copy: %v %v", got.Skipped, err)
	}
	c, ok := copyTemplateOf(t, d, got.Event.ID)
	if !ok || c.id == d.template || c.name != "Gecekodu 2027 sertifikası" || c.team != "WEBLAB" {
		t.Fatalf("copied template %+v %v", c, ok)
	}
	// YK reads the source template: the design's link comes along.
	if c.ref != "ref-1" || c.link != "https://figma.example/design" || !strings.Contains(c.draft, `"draft"`) {
		t.Fatalf("links and draft %+v", c)
	}
	var version int
	var checksum, manifest string
	var policy bool
	var publishedBy *uuid.UUID
	if err := d.pool.QueryRow(ctx, `SELECT version, checksum, asset_manifest::text, asset_serving_policy_applied, published_by
		FROM certificate_template_versions WHERE template_id = $1`, c.id).Scan(&version, &checksum, &manifest, &policy, &publishedBy); err != nil {
		t.Fatal(err)
	}
	if version != 1 || checksum != "sum-2" || !strings.Contains(manifest, "certificate-template-assets/v2/k") || !policy ||
		publishedBy == nil || publishedBy.String() != d.yk.ID {
		t.Fatalf("version %d %s %s %v %v", version, checksum, manifest, policy, publishedBy)
	}
	if n := d.count(t, `SELECT count(*) FROM certificate_template_bindings WHERE scope = 'event' AND scope_key = $1 AND template_id = $2`, got.Event.ID.String(), c.id); n != 1 {
		t.Fatalf("binding %d", n)
	}
	// Independent: the copy's draft changes, the source's does not.
	if _, err := d.pool.Exec(ctx, `UPDATE certificate_templates SET draft_layout = '{"label": "edited"}' WHERE id = $1`, c.id); err != nil {
		t.Fatal(err)
	}
	if n := d.count(t, `SELECT count(*) FROM certificate_templates WHERE id = $1 AND draft_layout->>'label' = 'draft'`, d.template); n != 1 {
		t.Fatal("source's draft changed")
	}
	if n := d.count(t, `SELECT count(*) FROM certificate_template_versions WHERE template_id = $1`, d.template); n != 2 {
		t.Fatalf("source's versions %d", n)
	}

	// Into another Owner team: the copy's template is that team's.
	in := copyIn("Gecekodu ARTLAB", "2027-03-13T19:00:00+03:00")
	in.OwnerTeam = "ARTLAB"
	got, _, err = d.svc.Copy(ctx, d.yk, d.source.ID, in)
	if err != nil || len(got.Skipped) != 0 {
		t.Fatalf("other team: %v %v", got.Skipped, err)
	}
	if c, ok := copyTemplateOf(t, d, got.Event.ID); !ok || c.team != "ARTLAB" {
		t.Fatalf("other team's template %+v", c)
	}
}

// Who does not read the source template (a WEBLAB member who edits and
// copies WEBLAB's Events under a policy that lets members) gets the layout,
// not the design's link; the copy is still bound to its own template.
func TestPostgresCopyEventTemplateLinksOnlyForReaders(t *testing.T) {
	d := newCopyDatabase(t)
	ctx := context.Background()
	sourceEventTemplate(t, d)
	policy := authz.DefaultPolicy()
	policy.EventPermissions["WEBLAB"] = policy.EventPermissions["GECEKODU"]
	svc := event.NewService(d.store, authz.NewAuthorizer(policy))
	member := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/WEBLAB"}}
	got, _, err := svc.Copy(ctx, member, d.source.ID, copyIn("Üye kopyası", "2027-03-13T19:00:00+03:00"))
	if err != nil || len(got.Skipped) != 0 {
		t.Fatalf("member copy: %v %v", got.Skipped, err)
	}
	c, ok := copyTemplateOf(t, d, got.Event.ID)
	if !ok || c.ref != "" || c.link != "" {
		t.Fatalf("member's copy %+v %v", c, ok)
	}
	if n := d.count(t, `SELECT count(*) FROM certificate_template_bindings WHERE scope_key = $1 AND template_id = $2`, got.Event.ID.String(), c.id); n != 1 {
		t.Fatalf("member's binding %d", n)
	}
}

// A draft-only Event template is copied but binds nothing; one whose Media
// is no longer current is left out and named; asked to leave it out, it is
// named and not written.
func TestPostgresCopyEventTemplateDraftsAndRefusals(t *testing.T) {
	d := newCopyDatabase(t)
	ctx := context.Background()
	sourceEventTemplate(t, d)
	if _, err := d.pool.Exec(ctx, `DELETE FROM certificate_template_versions WHERE template_id = $1`, d.template); err != nil {
		t.Fatal(err)
	}
	got, _, err := d.svc.Copy(ctx, d.yk, d.source.ID, copyIn("Taslak", "2027-03-13T19:00:00+03:00"))
	if err != nil || len(got.Skipped) != 0 {
		t.Fatalf("draft: %v %v", got.Skipped, err)
	}
	if _, ok := copyTemplateOf(t, d, got.Event.ID); !ok {
		t.Fatal("draft not copied")
	}
	if n := d.count(t, `SELECT count(*) FROM certificate_template_bindings WHERE scope_key = $1`, got.Event.ID.String()); n != 0 {
		t.Fatalf("draft bound: %d", n)
	}

	in := copyIn("Atla", "2027-03-13T19:00:00+03:00")
	in.SkipCertificateTemplate = true
	got, _, err = d.svc.Copy(ctx, d.yk, d.source.ID, in)
	if err != nil || !slices.Equal(got.Skipped, []string{event.SkippedCertificateTemplate}) {
		t.Fatalf("left out: %v %v", got.Skipped, err)
	}
	if _, ok := copyTemplateOf(t, d, got.Event.ID); ok {
		t.Fatal("left out but copied")
	}

	// The draft's background is deleted since: the database refuses the
	// copy's link, the Event copy goes on without it.
	background, err := media.NewPostgresStore(d.pool).Create(ctx, media.Media{Name: "arka.png", Type: "image/png", Kind: media.KindImage, Key: "images/arka", UploadedBy: uuid.MustParse(d.yk.ID)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.pool.Exec(ctx, `UPDATE certificate_templates SET draft_layout = jsonb_build_object('backgroundMediaId', $2::text) WHERE id = $1`, d.template, background.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.pool.Exec(ctx, `UPDATE media SET deleted_at = now() WHERE id = $1`, background.ID); err != nil {
		t.Fatal(err)
	}
	got, _, err = d.svc.Copy(ctx, d.yk, d.source.ID, copyIn("Silinmiş", "2027-03-13T19:00:00+03:00"))
	if err != nil || !slices.Equal(got.Skipped, []string{event.SkippedCertificateTemplate}) {
		t.Fatalf("refused media: %v %v", got.Skipped, err)
	}
	if _, ok := copyTemplateOf(t, d, got.Event.ID); ok {
		t.Fatal("refused template written")
	}
}

// The Event's template follows its Owner team in the same transaction.
func TestPostgresEventTemplateFollowsOwnerTeam(t *testing.T) {
	d := newCopyDatabase(t)
	ctx := context.Background()
	sourceEventTemplate(t, d)
	ev, err := d.store.Get(ctx, d.source.ID)
	if err != nil {
		t.Fatal(err)
	}
	ev.OwnerTeam = "ARTLAB"
	if _, err := d.store.Update(ctx, ev); err != nil {
		t.Fatal(err)
	}
	if n := d.count(t, `SELECT count(*) FROM certificate_templates WHERE id = $1 AND owner_team = 'ARTLAB'`, d.template); n != 1 {
		t.Fatal("template's Owner team not changed")
	}
}
