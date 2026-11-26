package retention_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/retention"
)

// guest_phone v2: a guest Ticket linked to an account
// (docs/guest-account-links.md) loses its phone 90 days after its Event as
// an unlinked guest's does; its name and e-mail stay (the account's), and
// guest_identity leaves it alone.
func TestGuestPhoneReachesLinkedGuestTickets(t *testing.T) {
	pool := migrated(t)
	ctx := context.Background()
	eventID, member := uuid.New(), uuid.New()
	exec(t, pool, `INSERT INTO users (id, email) VALUES ($1, 'ada@example.com')`, member)
	exec(t, pool, `INSERT INTO events (id, name, location, owner_team, end_date) VALUES ($1, 'old', 'YTÜ', 'SKY LAB', $2)`, eventID, testNow.Add(-800*day))
	linked, unlinked := uuid.New(), uuid.New()
	exec(t, pool, `INSERT INTO tickets (id, event_id, ticket_type, owner_id, guest_first_name, guest_email, guest_phone_number, owner_link_source, owner_linked_at)
		VALUES ($1, $2, 'GUEST', $3, 'Ada', 'ada@kisisel.org', '+905551112233', 'bulk', now())`, linked, eventID, member)
	exec(t, pool, `INSERT INTO tickets (id, event_id, ticket_type, guest_first_name, guest_email, guest_phone_number)
		VALUES ($1, $2, 'GUEST', 'Grace', 'grace@example.com', '+905554445566')`, unlinked, eventID)

	now := testNow
	report, err := sweeper(pool, dryRunConfig(), &now, nil).Run(ctx, retention.RunOptions{Mode: retention.ModeApply, Trigger: retention.TriggerCLI, Rule: "guest_phone"})
	if err != nil || report.Status != retention.RunOK || len(report.Rules) != 1 || report.Rules[0].Changed != 2 {
		t.Fatalf("%v %+v", err, report)
	}
	if phone := scalar[string](t, pool, `SELECT guest_phone_number FROM tickets WHERE id = $1`, linked); phone != "" {
		t.Fatalf("the linked Ticket kept its phone")
	}
	report, err = sweeper(pool, dryRunConfig(), &now, nil).Run(ctx, retention.RunOptions{Mode: retention.ModeApply, Trigger: retention.TriggerCLI, Rule: "guest_identity"})
	if err != nil || report.Status != retention.RunOK || len(report.Rules) != 1 || report.Rules[0].Changed != 1 {
		t.Fatalf("%v %+v", err, report)
	}
	if email := scalar[string](t, pool, `SELECT guest_email FROM tickets WHERE id = $1`, linked); email != "ada@kisisel.org" {
		t.Fatalf("guest_identity took the linked Ticket's address")
	}
}

// linked_guest_details: a linked guest Ticket's Walk-in university,
// department and grade go two years after its Event; its name and e-mail
// stay. An unlinked guest's go with its identity (guest_identity), not here.
func TestLinkedGuestDetailsGoAfterTwoYears(t *testing.T) {
	pool := migrated(t)
	ctx := context.Background()
	oldEvent, recentEvent, member := uuid.New(), uuid.New(), uuid.New()
	exec(t, pool, `INSERT INTO users (id, email) VALUES ($1, 'ada@example.com')`, member)
	exec(t, pool, `INSERT INTO events (id, name, location, owner_team, end_date) VALUES ($1, 'old', 'YTÜ', 'SKY LAB', $2)`, oldEvent, testNow.Add(-800*day))
	exec(t, pool, `INSERT INTO events (id, name, location, owner_team, end_date) VALUES ($1, 'recent', 'YTÜ', 'SKY LAB', $2)`, recentEvent, testNow.Add(-10*day))
	linkedOld, linkedRecent, unlinked := uuid.New(), uuid.New(), uuid.New()
	for _, row := range []struct {
		id, event uuid.UUID
		owner     *uuid.UUID
		email     string
	}{{linkedOld, oldEvent, &member, "ada@kisisel.org"}, {linkedRecent, recentEvent, &member, "ada@kisisel.org"}, {unlinked, oldEvent, nil, "grace@example.com"}} {
		exec(t, pool, `INSERT INTO tickets (id, event_id, ticket_type, owner_id, guest_first_name, guest_email, guest_university, guest_department, guest_grade, owner_link_source, owner_linked_at)
			VALUES ($1, $2, 'GUEST', $3, 'Ad', $4, 'YTÜ', 'Matematik', '2', CASE WHEN $3::uuid IS NULL THEN NULL ELSE 'sync' END, CASE WHEN $3::uuid IS NULL THEN NULL ELSE now() END)`,
			row.id, row.event, row.owner, row.email)
	}
	now := testNow
	report, err := sweeper(pool, dryRunConfig(), &now, nil).Run(ctx, retention.RunOptions{Mode: retention.ModeApply, Trigger: retention.TriggerCLI, Rule: "linked_guest_details"})
	if err != nil || report.Status != retention.RunOK || len(report.Rules) != 1 || report.Rules[0].Changed != 1 {
		t.Fatalf("%v %+v", err, report)
	}
	details := func(id uuid.UUID) string {
		return scalar[string](t, pool, `SELECT concat_ws('|', guest_first_name, guest_email, guest_university, guest_department, guest_grade) FROM tickets WHERE id = $1`, id)
	}
	if got := details(linkedOld); got != "Ad|ada@kisisel.org|||" {
		t.Fatalf("linked, two years on: %q", got)
	}
	if got := details(linkedRecent); got != "Ad|ada@kisisel.org|YTÜ|Matematik|2" {
		t.Fatalf("linked, recent: %q", got)
	}
	if got := details(unlinked); got != "Ad|grace@example.com|YTÜ|Matematik|2" {
		t.Fatalf("unlinked guest changed by the linked rule: %q", got)
	}
}
