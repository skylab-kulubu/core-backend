package dashboard_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/dashboard"
)

func TestPostgresStoreHome(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	store := dashboard.NewPostgresStore(pool)

	a, b, other := uuid.New(), uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{a, b, other} {
		exec(t, pool, `INSERT INTO events (id, name, location, owner_team) VALUES ($1, 'E', 'L', 'WEBLAB')`, id)
	}
	base := time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)
	day, archivedDay, otherDay := uuid.New(), uuid.New(), uuid.New()
	exec(t, pool, `INSERT INTO event_days (id, event_id) VALUES ($1, $2)`, day, a)
	exec(t, pool, `INSERT INTO event_days (id, event_id, archived_at) VALUES ($1, $2, now())`, archivedDay, a)
	exec(t, pool, `INSERT INTO event_days (id, event_id) VALUES ($1, $2)`, otherDay, other)
	session := func(dayID uuid.UUID, title string, start time.Time, extra string) uuid.UUID {
		id := uuid.New()
		exec(t, pool, `INSERT INTO sessions (id, event_day_id, title, session_type, start_time, end_time) VALUES ($1, $2, $3, 'TALK', $4, $5)`,
			id, dayID, title, start, start.Add(time.Hour))
		if extra != "" {
			exec(t, pool, `UPDATE sessions SET `+extra+` WHERE id = $1`, id)
		}
		return id
	}
	first := session(day, "Açılış", base, "")
	second := session(day, "Kapanış", base.Add(3*time.Hour), "")
	session(day, "İptal", base.Add(time.Hour), "cancelled = true")
	session(day, "Arşiv", base.Add(time.Hour), "archived_at = now()")
	session(archivedDay, "Arşiv gün", base.Add(time.Hour), "")
	session(day, "Sonra", base.Add(10*24*time.Hour), "")
	session(otherDay, "Başka", base, "")

	counts, err := store.SessionCounts(ctx, []uuid.UUID{a, b})
	if err != nil {
		t.Fatal(err)
	}
	if counts[a] != 3 || counts[b] != 0 || len(counts) != 1 {
		t.Fatalf("session counts %v", counts)
	}
	slots, err := store.SessionsBetween(ctx, []uuid.UUID{a, b}, base, base.Add(7*24*time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(slots) != 2 || slots[0].ID != first || slots[1].ID != second || slots[0].EventID != a || slots[0].Title != "Açılış" ||
		!slots[0].StartTime.Equal(base) || slots[0].EndTime == nil || !slots[0].EndTime.Equal(base.Add(time.Hour)) {
		t.Fatalf("slots %+v", slots)
	}
	if limited, err := store.SessionsBetween(ctx, []uuid.UUID{a}, base, base.Add(7*24*time.Hour), 1); err != nil || len(limited) != 1 {
		t.Fatalf("limit %v %v", limited, err)
	}

	// Tickets and check-ins: two periods of 30 days up to base.
	mid, from := base.Add(-30*24*time.Hour), base.Add(-60*24*time.Hour)
	ticket := func(eventID uuid.UUID, created time.Time) uuid.UUID {
		id := uuid.New()
		exec(t, pool, `INSERT INTO tickets (id, event_id, ticket_type, guest_email, created_at) VALUES ($1, $2, 'GUEST', $3, $4)`,
			id, eventID, id.String()+"@example.com", created)
		return id
	}
	recent := ticket(a, base.Add(-time.Hour))
	ticket(b, base.Add(-10*24*time.Hour))
	older := ticket(a, base.Add(-40*24*time.Hour))
	ticket(a, base.Add(-70*24*time.Hour))
	ticket(other, base.Add(-time.Hour))
	checkIn := func(ticketID, sessionID uuid.UUID, at time.Time) {
		exec(t, pool, `INSERT INTO ticket_checkins (id, ticket_id, event_day_id, session_id, created_at) VALUES ($1, $2, $3, $4, $5)`,
			uuid.New(), ticketID, day, sessionID, at)
	}
	// Twice in the current period: one attendee.
	checkIn(recent, first, base.Add(-30*time.Minute))
	checkIn(recent, second, base.Add(-20*time.Minute))
	checkIn(older, first, base.Add(-39*24*time.Hour))
	istanbul, err := time.LoadLocation("Europe/Istanbul")
	if err != nil {
		t.Fatal(err)
	}
	checkIns, err := store.DailyCheckIns(ctx, []uuid.UUID{a, b}, from, istanbul)
	if err != nil {
		t.Fatal(err)
	}
	if len(checkIns) != 1 || checkIns[a]["2026-10-03"] != 1 || checkIns[a]["2026-08-25"] != 1 || len(checkIns[a]) != 2 {
		t.Fatalf("check-ins %v", checkIns)
	}
	if later, err := store.DailyCheckIns(ctx, []uuid.UUID{a}, mid, istanbul); err != nil || len(later[a]) != 1 {
		t.Fatalf("since %v %v", later, err)
	}

	// Certificates: two issued now, one of them revoked, one 40 days ago,
	// one of another Event.
	certificate := func(eventID, ticketID uuid.UUID, issued time.Time, revoked bool) {
		var revokedAt any
		if revoked {
			revokedAt = issued
		}
		exec(t, pool, `INSERT INTO certificates (id, event_id, ticket_id, serial, recipient_name, recipient_email, event_name, owner_team, verify_url, issued_at, revoked_at)
			VALUES ($1, $2, $3, $4, 'Ada', 'ada@example.test', 'E', 'WEBLAB', 'https://example.test', $5, $6)`,
			uuid.New(), eventID, ticketID, uuid.NewString(), issued, revokedAt)
	}
	certificate(a, recent, base.Add(-time.Hour), false)
	certificate(a, older, base.Add(-2*time.Hour), true)
	certificate(a, ticket(a, base.Add(-50*24*time.Hour)), base.Add(-40*24*time.Hour), false)
	certificate(other, ticket(other, base), base, false)
	certs, err := store.CertificateCounts(ctx, []uuid.UUID{a, b}, from, mid)
	if err != nil {
		t.Fatal(err)
	}
	if certs != (dashboard.CertificateCounts{Issued: 3, Revoked: 1, Period: dashboard.Period{Current: 2, Previous: 1}}) {
		t.Fatalf("certificates %+v", certs)
	}

	// Failed certificate jobs, by Event.
	batch := uuid.New()
	exec(t, pool, `INSERT INTO certificate_batches (id, event_id, template_version_id, template_source, reason)
		VALUES ($1, $2, '89c0bfe1-5be5-4ddb-8686-4865f51e3401', 'clubDefault', 'reissue')`, batch, a)
	for _, status := range []string{"failed", "failed", "issued", "queued"} {
		exec(t, pool, `INSERT INTO certificate_jobs (id, batch_id, event_id, ticket_id, status) VALUES ($1, $2, $3, $4, $5)`,
			uuid.New(), batch, a, ticket(a, base.Add(-80*24*time.Hour)), status)
	}
	failed, err := store.FailedCertificateJobs(ctx, []uuid.UUID{a, b})
	if err != nil {
		t.Fatal(err)
	}
	if failed[a] != 2 || len(failed) != 1 {
		t.Fatalf("failed %v", failed)
	}

	// Open data subject requests: received ten and forty days ago, one
	// awaiting verification, one answered.
	request := func(received time.Time, status string, answered any) {
		exec(t, pool, `
			INSERT INTO subject_requests (id, received_on, channel, requester_name, requester_contact, request_types,
				status, answered_on, created_at, updated_at)
			VALUES ($1, $2::date, 'email', 'Ada Lovelace', 'ada@example.test', '{erase}', $3, $4::date, now(), now())`,
			uuid.New(), received.Format(time.DateOnly), status, answered)
	}
	request(base.Add(-10*24*time.Hour), "received", nil)
	request(base.Add(-40*24*time.Hour), "received", nil)
	request(base.Add(-2*24*time.Hour), "awaiting_verification", nil)
	request(base.Add(-50*24*time.Hour), "answered", base.Add(-45*24*time.Hour).Format(time.DateOnly))
	requests, err := store.OpenSubjectRequests(ctx, base.Format(time.DateOnly))
	if err != nil {
		t.Fatal(err)
	}
	if requests != (dashboard.SubjectRequestCounts{Open: 3, Overdue: 1}) {
		t.Fatalf("subject requests %+v", requests)
	}
}
