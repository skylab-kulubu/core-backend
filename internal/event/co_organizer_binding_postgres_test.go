package event_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
)

// An Event bound to the template of a Co-organizer team shown outside the
// panel loses that binding when the team leaves the list or becomes a
// helper, in the same write (docs/co-organizers.md); a binding to the
// Owner team's template stays.
func TestCoOrganizerTemplateBindingGoesWithTheTeam(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := event.NewPostgresStore(pool)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	bound := func(id uuid.UUID) string {
		t.Helper()
		var team string
		err := pool.QueryRow(ctx, `SELECT t.owner_team FROM certificate_template_bindings b JOIN certificate_templates t ON t.id = b.template_id
			WHERE b.scope = 'event' AND b.scope_key = $1::text`, id.String()).Scan(&team)
		if err != nil {
			return ""
		}
		return team
	}
	weblab, artlab := uuid.New(), uuid.New()
	exec(`INSERT INTO certificate_templates (id, name, owner_team) VALUES ($1, 'WEBLAB', 'WEBLAB'), ($2, 'ARTLAB', 'ARTLAB')`, weblab, artlab)
	newEvent := func(template uuid.UUID) uuid.UUID {
		t.Helper()
		id := uuid.New()
		exec(`INSERT INTO events (id, name, location, owner_team) VALUES ($1, 'Hack', 'YTÜ', 'ARTLAB')`, id)
		exec(`INSERT INTO event_co_organizers (event_id, team_code, management, visibility) VALUES ($1, 'WEBLAB', 'leaders', 'co_organizer')`, id)
		exec(`INSERT INTO certificate_template_bindings (id, scope, scope_key, template_id) VALUES ($1, 'event', $2, $3)`, uuid.New(), id.String(), template)
		return id
	}
	removed, helper, owners := newEvent(weblab), newEvent(weblab), newEvent(artlab)

	if _, err := store.SetCoOrganizers(ctx, event.CoOrganizersWrite{EventID: removed, OwnerTeam: "ARTLAB"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetCoOrganizers(ctx, event.CoOrganizersWrite{EventID: helper, OwnerTeam: "ARTLAB",
		Teams: []event.CoOrganizerTeam{{Code: "WEBLAB", Management: event.ManagementLeaders, Visibility: event.VisibilityHelper}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetCoOrganizers(ctx, event.CoOrganizersWrite{EventID: owners, OwnerTeam: "ARTLAB"}); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		id   uuid.UUID
		want string
	}{"taken off": {removed, ""}, "made a helper": {helper, ""}, "the Owner team's": {owners, "ARTLAB"}} {
		if got := bound(c.id); got != c.want {
			t.Errorf("%s: bound to %q, want %q", name, got, c.want)
		}
	}
}
