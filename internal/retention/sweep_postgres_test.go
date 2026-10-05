package retention_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/retention"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
)

const day = 24 * time.Hour

// testNow is the sweep's clock in these tests; the fixture's ages count back
// from it.
var testNow = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

func migrated(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := testpostgres.Start(t)
	if err := migrate.Apply(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	return pool
}

func exec(t *testing.T, pool *pgxpool.Pool, query string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), query, args...); err != nil {
		t.Fatalf("%v\n%s", err, query)
	}
}

func scalar[T any](t *testing.T, pool *pgxpool.Pool, query string, args ...any) T {
	t.Helper()
	var v T
	if err := pool.QueryRow(context.Background(), query, args...).Scan(&v); err != nil {
		t.Fatalf("%v\n%s", err, query)
	}
	return v
}

// lines collects a sweeper's log lines.
type lines struct {
	mu  sync.Mutex
	all []string
}

func (l *lines) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.all = append(l.all, fmt.Sprintf(format, args...))
}

func (l *lines) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.all, "\n")
}

func sweeper(pool *pgxpool.Pool, config retention.Config, now *time.Time, log *lines) *retention.Sweeper {
	logf := func(string, ...any) {}
	if log != nil {
		logf = log.logf
	}
	s := retention.NewSweeper(pool, config, logf)
	s.SetClock(func() time.Time { return *now })
	return s
}

func dryRunConfig() retention.Config {
	return retention.Config{Mode: retention.ModeDryRun, Period: 90 * day, MediaRecoveryWindow: 30 * day}
}

// world is a fixture with something due, and something not, for every rule.
// The personal values in it are distinctive, so a test can show none of them
// reached a record or a log line.
type world struct {
	member                                                   uuid.UUID
	eOld, eMid, eRecent, eDays, eStartOnly, eNoAnchor, eArch uuid.UUID
	gOldOnly, gBackOld, gBackRecent, gNoEmail, gPendingCert  uuid.UUID
	gCertified, gNoAnchor, gArchived, gMid, gDays, gStart    uuid.UUID
	gRecent, memberTicket, certificate, checkIn              uuid.UUID
	hitDue, hitScrubbed, hitYoung, hitFresh                  uuid.UUID
	link                                                     uuid.UUID
}

// personal are the fixture's personal values.
var personal = []string{
	"Old.Only@Example.com", "old.only@example.com", "Ada", "Lovelace", "back.again@example.com", "Back.Again@Example.COM", "noemail-first",
	"pending@example.com", "cert@example.com", "Cert Person", "noanchor@example.com", "archived@example.com",
	"mid@example.com", "days@example.com", "start@example.com", "recent@example.com", "+90555",
	"203.0.113.", "198.51.100.", "Mozilla/5.0 (Fixture)", "/private/path", "secret-token", "member@example.com",
}

func seed(t *testing.T, pool *pgxpool.Pool) world {
	t.Helper()
	w := world{member: uuid.New()}
	exec(t, pool, `INSERT INTO users (id, email, first_name, last_name) VALUES ($1, 'member@example.com', 'Member', 'Person')`, w.member)

	event := func(name string, end, dayEnd, start *time.Time, archived bool) uuid.UUID {
		t.Helper()
		id := uuid.New()
		exec(t, pool, `INSERT INTO events (id, name, location, owner_team, start_date, end_date, archived_at)
			VALUES ($1, $2, 'YTÜ', 'SKY LAB', $3, $4, CASE WHEN $5 THEN now() END)`, id, name, start, end, archived)
		if dayEnd != nil {
			exec(t, pool, `INSERT INTO event_days (id, event_id, name, start_date, end_date) VALUES ($1, $2, 'Gün', $3, $3)`, uuid.New(), id, dayEnd)
		}
		return id
	}
	ago := func(d time.Duration) *time.Time { at := testNow.Add(-d); return &at }
	w.eOld = event("old", ago(800*day), nil, ago(801*day), false)
	w.eMid = event("mid", ago(100*day), nil, ago(101*day), false)
	w.eRecent = event("recent", ago(10*day), nil, ago(11*day), false)
	w.eDays = event("days", nil, ago(200*day), ago(201*day), false)
	w.eStartOnly = event("start", nil, nil, ago(95*day), false)
	w.eNoAnchor = event("no anchor", nil, nil, nil, false)
	w.eArch = event("archived", ago(800*day), nil, ago(801*day), true)

	guest := func(eventID uuid.UUID, first, last, email string) uuid.UUID {
		t.Helper()
		id := uuid.New()
		exec(t, pool, `INSERT INTO tickets (id, event_id, ticket_type, guest_first_name, guest_last_name, guest_email, guest_phone_number)
			VALUES ($1, $2, 'GUEST', $3, $4, $5, '+905551112233')`, id, eventID, first, last, email)
		return id
	}
	// Stored before guest apply normalized addresses: still one person.
	w.gOldOnly = guest(w.eOld, "Ada", "Lovelace", " Old.Only@Example.com")
	// One person, two spellings of one address: the recent Event keeps the
	// old Ticket's identity too.
	w.gBackOld = guest(w.eOld, "Back", "Again", " Back.Again@Example.COM")
	w.gBackRecent = guest(w.eRecent, "Back", "Again", "back.again@example.com")
	w.gNoEmail = guest(w.eOld, "noemail-first", "noemail-last", "")
	w.gPendingCert = guest(w.eOld, "Pending", "Cert", "pending@example.com")
	w.gCertified = guest(w.eOld, "Cert", "Person", "cert@example.com")
	w.gNoAnchor = guest(w.eNoAnchor, "No", "Anchor", "noanchor@example.com")
	w.gArchived = guest(w.eArch, "Arch", "Ived", "archived@example.com")
	w.gMid = guest(w.eMid, "Mid", "Guest", "mid@example.com")
	w.gDays = guest(w.eDays, "Days", "Guest", "days@example.com")
	w.gStart = guest(w.eStartOnly, "Start", "Guest", "start@example.com")
	w.gRecent = guest(w.eRecent, "Recent", "Guest", "recent@example.com")
	w.memberTicket = uuid.New()
	exec(t, pool, `INSERT INTO tickets (id, event_id, ticket_type, owner_id, guest_phone_number) VALUES ($1, $2, 'REGISTERED', $3, '+905559998877')`,
		w.memberTicket, w.eOld, w.member)

	// The certified guest's check-in and certificate stay; only the
	// certificate's address goes.
	dayID, sessionID := uuid.New(), uuid.New()
	exec(t, pool, `INSERT INTO event_days (id, event_id, name) VALUES ($1, $2, 'Gün 1')`, dayID, w.eOld)
	exec(t, pool, `INSERT INTO sessions (id, event_day_id, title, session_type) VALUES ($1, $2, 'Açılış', 'TALK')`, sessionID, dayID)
	w.checkIn = uuid.New()
	exec(t, pool, `INSERT INTO ticket_checkins (id, ticket_id, event_day_id, session_id) VALUES ($1, $2, $3, $4)`, w.checkIn, w.gCertified, dayID, sessionID)
	w.certificate = uuid.New()
	exec(t, pool, `INSERT INTO certificates (id, event_id, ticket_id, serial, recipient_name, recipient_email, event_name, owner_team, verify_url)
		VALUES ($1, $2, $3, 'SKY-CERT-1', 'Cert Person', 'cert@example.com', 'old', 'SKY LAB', 'https://verify.example/SKY-CERT-1')`,
		w.certificate, w.eOld, w.gCertified)
	// An owned certificate on the same Ticket (an old, revoked one) is the
	// owner's: the guest rule leaves it alone.
	exec(t, pool, `INSERT INTO certificates (id, event_id, ticket_id, owner_id, serial, recipient_name, recipient_email, event_name, owner_team, verify_url, revoked_at)
		VALUES ($1, $2, $3, $4, 'SKY-CERT-0', 'Member Person', 'member@example.com', 'old', 'SKY LAB', 'https://verify.example/SKY-CERT-0', now())`,
		uuid.New(), w.eOld, w.gCertified, w.member)
	// A certificate still being issued from the pending guest's name.
	batchID := uuid.New()
	exec(t, pool, `INSERT INTO certificate_batches (id, event_id, template_version_id, template_source, reason)
		VALUES ($1, $2, '89c0bfe1-5be5-4ddb-8686-4865f51e3401', 'clubDefault', 'manual')`, batchID, w.eOld)
	exec(t, pool, `INSERT INTO certificate_jobs (id, batch_id, event_id, ticket_id, status) VALUES ($1, $2, $3, $4, 'queued')`,
		uuid.New(), batchID, w.eOld, w.gPendingCert)
	// An issued job of the certified guest holds nothing back.
	exec(t, pool, `INSERT INTO certificate_jobs (id, batch_id, event_id, ticket_id, status, certificate_id) VALUES ($1, $2, $3, $4, 'issued', $5)`,
		uuid.New(), batchID, w.eOld, w.gCertified, w.certificate)

	for _, eventID := range []uuid.UUID{w.eOld, w.eMid, w.eRecent, w.eNoAnchor} {
		exec(t, pool, `INSERT INTO event_door_staff (event_id, user_id) VALUES ($1, $2)`, eventID, w.member)
	}

	urlID := uuid.New()
	exec(t, pool, `INSERT INTO urls (id, alias, url) VALUES ($1, 'fixture', 'https://forms.example/f')`, urlID)
	hit := func(age time.Duration, ip, agent, referer string, member bool) uuid.UUID {
		t.Helper()
		id := uuid.New()
		var userID *uuid.UUID
		if member {
			userID = &w.member
		}
		exec(t, pool, `INSERT INTO url_hits (id, url_id, alias, at, ip, user_agent, referer, user_id, utm_source)
			VALUES ($1, $2, 'fixture', $3, $4, $5, $6, $7, 'instagram')`, id, urlID, testNow.Add(-age), ip, agent, referer, userID)
		return id
	}
	w.hitDue = hit(400*day, "203.0.113.7", "Mozilla/5.0 (Fixture)", "https://User:Pw@Example.COM:8443/private/path?token=secret-token#x", true)
	w.hitScrubbed = hit(400*day, "", "", "https://example.com", false)
	w.hitYoung = hit(100*day, "203.0.113.8", "Mozilla/5.0 (Fixture)", "https://example.com/private/path", true)
	w.hitFresh = hit(10*day, "203.0.113.9", "Mozilla/5.0 (Fixture)", "", false)

	mediaID := uuid.New()
	exec(t, pool, `INSERT INTO media (id, file_name, file_type, file_url, file_size, uploaded_by, kind)
		VALUES ($1, 'cv.pdf', 'application/pdf', 'files/cv.pdf', 3, $2, 'FILE')`, mediaID, w.member)
	w.link = uuid.New()
	issued := testNow.Add(-400 * day)
	exec(t, pool, `INSERT INTO media_read_links (id, media_id, product, on_behalf_of, issued_at, expires_at) VALUES ($1, $2, 'forms', $3, $4, $5)`,
		w.link, mediaID, w.member, issued, issued.Add(5*time.Minute))
	exec(t, pool, `INSERT INTO media_read_link_opens (link_id, opened_at, client_ip) VALUES ($1, $2, '198.51.100.4'), ($1, $2, '')`, w.link, issued.Add(time.Minute))
	// An archived Media past its 30 days whose object is still there.
	exec(t, pool, `INSERT INTO media (id, file_name, file_type, file_url, file_size, uploaded_by, kind, deleted_at)
		VALUES ($1, 'old.png', 'image/png', 'images/old.png', 3, $2, 'IMAGE', $3)`, uuid.New(), w.member, testNow.Add(-40*day))
	exec(t, pool, `INSERT INTO event_mail_snapshots (mail_list_id, event_id, expires_at) VALUES ($1, $2, $3), ($4, $2, $5)`,
		uuid.New(), w.eRecent, testNow.Add(-2*day), uuid.New(), testNow.Add(2*day))
	return w
}

// state is every column the rules may touch, for comparing before and after.
func state(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	return scalar[string](t, pool, `SELECT concat_ws(E'\n',
		(SELECT string_agg(concat_ws('|', id, guest_first_name, guest_last_name, guest_email, guest_phone_number, updated_at), E'\n' ORDER BY id) FROM tickets),
		(SELECT string_agg(concat_ws('|', id, recipient_name, recipient_email, serial), E'\n' ORDER BY id) FROM certificates),
		(SELECT string_agg(concat_ws('|', event_id, user_id), E'\n' ORDER BY event_id) FROM event_door_staff),
		(SELECT string_agg(concat_ws('|', id, ip, user_agent, referer, user_id), E'\n' ORDER BY id) FROM url_hits),
		(SELECT string_agg(concat_ws('|', link_id, opened_at, client_ip), E'\n' ORDER BY client_ip) FROM media_read_link_opens),
		(SELECT count(*)::text FROM ticket_checkins))`)
}

func results(report retention.Report) map[string]retention.RuleResult {
	out := map[string]retention.RuleResult{}
	for _, result := range report.Rules {
		out[result.Rule.Name] = result
	}
	return out
}

type counts struct {
	matched, changed, related, overdue, anchorless int64
	status                                         retention.RuleStatus
}

func check(t *testing.T, label string, report retention.Report, want map[string]counts) {
	t.Helper()
	got := results(report)
	if len(got) != len(want) {
		t.Fatalf("%s: %d rules, want %d", label, len(got), len(want))
	}
	for name, w := range want {
		r, ok := got[name]
		if !ok {
			t.Fatalf("%s: no %s", label, name)
		}
		c := counts{r.Matched, r.Changed, r.RelatedChanged, r.Overdue, r.Anchorless, r.Status}
		if c != w {
			t.Errorf("%s %s: got %+v, want %+v", label, name, c, w)
		}
	}
}

// The dry run changes nothing and counts what apply then changes, row for
// row; a second apply finds nothing. Guests are emptied, not deleted: their
// Tickets, check-ins and certificates stay (ADR-0042).
func TestDryRunCountsWhatApplyChangesAndApplyIsIdempotent(t *testing.T) {
	pool := migrated(t)
	w := seed(t, pool)
	ctx := context.Background()
	now := testNow
	log := &lines{}
	s := sweeper(pool, dryRunConfig(), &now, log)

	before := state(t, pool)
	dry, err := s.Run(ctx, retention.RunOptions{Mode: retention.ModeDryRun, Trigger: retention.TriggerSchedule})
	if err != nil || dry.Status != retention.RunOK {
		t.Fatalf("dry run: %v %+v", err, dry)
	}
	if after := state(t, pool); after != before {
		t.Fatalf("the dry run changed rows:\n%s\n---\n%s", before, after)
	}
	audits := map[string]counts{
		"url_hits_age":           {3, 0, 0, 3, 0, retention.RuleOK},
		"read_links_age":         {1, 0, 0, 1, 0, retention.RuleOK},
		"read_link_opens_age":    {2, 0, 0, 2, 0, retention.RuleOK},
		"mail_snapshots_age":     {1, 0, 0, 1, 0, retention.RuleOK},
		"media_archived_objects": {1, 0, 0, 1, 0, retention.RuleOK},
		"media_expired_objects":  {0, 0, 0, 0, 0, retention.RuleOK},
	}
	want := map[string]counts{
		// Phones of guests whose Event ended more than 90 days ago: old,
		// back-old, no e-mail, pending, certified, archived, mid, days,
		// start. The Event without a date is counted, not changed.
		"guest_phone": {9, 0, 0, 9, 1, retention.RuleDryRun},
		// Identities two years past the person's latest Event: old, no
		// e-mail (its own Event), certified, archived Event.
		"guest_identity": {4, 0, 0, 4, 1, retention.RuleDryRun},
		"door_staff":     {2, 0, 0, 2, 1, retention.RuleDryRun},
		"url_hits_scrub": {1, 0, 0, 1, 0, retention.RuleDryRun},
		"read_link_ip":   {1, 0, 0, 1, 0, retention.RuleDryRun},
	}
	for name, c := range audits {
		want[name] = c
	}
	for _, name := range []string{"consent_pending", "consent_renewal_unanswered", "consent_proof"} {
		want[name] = counts{0, 0, 0, 0, 0, retention.RuleDryRun}
	}
	check(t, "dry run", dry, want)

	apply, err := s.Run(ctx, retention.RunOptions{Mode: retention.ModeApply, Trigger: retention.TriggerSchedule})
	if err != nil || apply.Status != retention.RunOK {
		t.Fatalf("apply: %v %+v", err, apply)
	}
	applied := map[string]counts{
		"guest_phone":    {9, 9, 0, 0, 1, retention.RuleOK},
		"guest_identity": {4, 4, 1, 0, 1, retention.RuleOK},
		"door_staff":     {2, 2, 0, 0, 1, retention.RuleOK},
		"url_hits_scrub": {1, 1, 0, 0, 0, retention.RuleOK},
		"read_link_ip":   {1, 1, 0, 0, 0, retention.RuleOK},
	}
	for name, c := range audits {
		applied[name] = c
	}
	for _, name := range []string{"consent_pending", "consent_renewal_unanswered", "consent_proof"} {
		applied[name] = counts{0, 0, 0, 0, 0, retention.RuleOK}
	}
	check(t, "apply", apply, applied)
	for name, r := range results(dry) {
		if r.Rule.Kind == retention.KindSweep && results(apply)[name].Changed != r.Matched {
			t.Errorf("%s: dry run counted %d, apply changed %d", name, r.Matched, results(apply)[name].Changed)
		}
	}

	identity := func(id uuid.UUID) string {
		return scalar[string](t, pool, `SELECT concat_ws('|', guest_first_name, guest_last_name, guest_email, guest_phone_number) FROM tickets WHERE id = $1`, id)
	}
	for id, want := range map[uuid.UUID]string{
		w.gOldOnly:     "|||",
		w.gNoEmail:     "|||",
		w.gCertified:   "|||",
		w.gArchived:    "|||",
		w.gBackOld:     "Back|Again| Back.Again@Example.COM|",
		w.gBackRecent:  "Back|Again|back.again@example.com|+905551112233",
		w.gPendingCert: "Pending|Cert|pending@example.com|",
		w.gMid:         "Mid|Guest|mid@example.com|",
		w.gDays:        "Days|Guest|days@example.com|",
		w.gStart:       "Start|Guest|start@example.com|",
		w.gNoAnchor:    "No|Anchor|noanchor@example.com|+905551112233",
		w.gRecent:      "Recent|Guest|recent@example.com|+905551112233",
		w.memberTicket: "|||+905559998877",
	} {
		if got := identity(id); got != want {
			t.Errorf("ticket %s: %q, want %q", id, got, want)
		}
	}
	if got := scalar[int](t, pool, `SELECT count(*) FROM tickets`); got != 13 {
		t.Fatalf("tickets %d: a Ticket was deleted", got)
	}
	if got := scalar[string](t, pool, `SELECT concat_ws('|', recipient_name, recipient_email, serial, (SELECT count(*) FROM ticket_checkins WHERE id = $2)) FROM certificates WHERE id = $1`,
		w.certificate, w.checkIn); got != "Cert Person||SKY-CERT-1|1" {
		t.Fatalf("certificate and check-in %q", got)
	}
	if got := scalar[string](t, pool, `SELECT recipient_email FROM certificates WHERE serial = 'SKY-CERT-0'`); got != "member@example.com" {
		t.Fatalf("owned certificate %q", got)
	}
	if got := scalar[string](t, pool, `SELECT string_agg(event_id::text, ',' ORDER BY event_id) FROM event_door_staff`); got != strings.Join(sortedIDs(w.eRecent, w.eNoAnchor), ",") {
		t.Fatalf("door staff left %q", got)
	}
	hit := func(id uuid.UUID) string {
		return scalar[string](t, pool, `SELECT concat_ws('|', ip, user_agent, referer, COALESCE(user_id::text, 'null'), utm_source, alias) FROM url_hits WHERE id = $1`, id)
	}
	// The row stays, with its time, link and channel; the referer keeps its
	// origin only.
	if got := hit(w.hitDue); got != "||https://example.com|null|instagram|fixture" {
		t.Fatalf("scrubbed hit %q", got)
	}
	if got := hit(w.hitScrubbed); got != "||https://example.com|null|instagram|fixture" {
		t.Fatalf("already scrubbed hit %q", got)
	}
	if got := hit(w.hitYoung); got != "203.0.113.8|Mozilla/5.0 (Fixture)|https://example.com/private/path|"+w.member.String()+"|instagram|fixture" {
		t.Fatalf("young hit %q", got)
	}
	if got := scalar[string](t, pool, `SELECT string_agg(client_ip, ',') FROM media_read_link_opens`); got != "," {
		t.Fatalf("opens %q: both rows stay, without an address", got)
	}

	again, err := s.Run(ctx, retention.RunOptions{Mode: retention.ModeApply, Trigger: retention.TriggerSchedule})
	if err != nil || again.Status != retention.RunOK {
		t.Fatalf("second apply: %v %+v", err, again)
	}
	for _, r := range again.Rules {
		if r.Rule.Kind == retention.KindSweep && (r.Matched != 0 || r.Changed != 0 || r.Overdue != 0) {
			t.Errorf("second apply %s: %+v", r.Rule.Name, r)
		}
	}

	// The records hold names, times and counts: none of the fixture's
	// personal values, and neither do the log lines.
	records := scalar[string](t, pool, `SELECT concat_ws(E'\n',
		(SELECT string_agg(row_to_json(r)::text, E'\n') FROM retention_runs r),
		(SELECT string_agg(row_to_json(r)::text, E'\n') FROM retention_run_rules r),
		(SELECT string_agg(row_to_json(p)::text, E'\n') FROM retention_periods p))`)
	for _, value := range personal {
		if strings.Contains(records, value) || strings.Contains(log.text(), value) {
			t.Errorf("%q reached a record or a log line", value)
		}
	}
	for _, id := range []uuid.UUID{w.gOldOnly, w.eOld, w.member, w.hitDue, w.link} {
		if strings.Contains(records, id.String()) || strings.Contains(log.text(), id.String()) {
			t.Errorf("row id %s reached a record or a log line", id)
		}
	}
	if got := scalar[int](t, pool, `SELECT count(*) FROM retention_run_rules`); got != 42 {
		t.Fatalf("rule records %d", got)
	}
	if got := scalar[string](t, pool, `SELECT string_agg(mode || ':' || status || ':' || triggered_by, ',' ORDER BY started_at, mode DESC) FROM retention_runs`); got != "dry-run:ok:schedule,apply:ok:schedule,apply:ok:schedule" {
		t.Fatalf("runs %q", got)
	}
	if !strings.Contains(log.text(), "retention_rule rule=guest_identity version=1 mode=apply status=ok cutoff=2024-10-05T12:00:00Z matched=4 changed=4 related=1 overdue=0 anchorless=1 code=-") {
		t.Fatalf("log:\n%s", log.text())
	}
}

func sortedIDs(ids ...uuid.UUID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// grant is a contact consent row for the tests: an address grant (email set)
// or an account's own (user set), open or ended, confirmed or pending.
type grant struct {
	purpose                  string
	email                    string
	user                     *uuid.UUID
	granted, confirmed, sent *time.Time
	ended, renewalRequested  *time.Time
	endedReason              string
}

func insertGrant(t *testing.T, pool *pgxpool.Pool, g grant) uuid.UUID {
	t.Helper()
	id := uuid.New()
	var email *string
	var hmac []byte
	if g.user == nil {
		hmac = make([]byte, 32)
		copy(hmac, id[:])
		if g.ended == nil {
			email = &g.email
		}
	}
	var confirmedVia, endedVia, endedReason *string
	if g.confirmed != nil {
		via := "link"
		confirmedVia = &via
	}
	if g.ended != nil {
		reason, via := g.endedReason, "link"
		if reason == "" {
			reason = "withdrawn"
		}
		if reason == "superseded" {
			via = "service"
		}
		endedReason, endedVia = &reason, &via
	}
	granted := testNow.Add(-day)
	if g.granted != nil {
		granted = *g.granted
	}
	purpose := g.purpose
	if purpose == "" {
		purpose = "event_invitations"
	}
	exec(t, pool, `INSERT INTO contact_consents (id, purpose, user_id, email, email_hmac, source, text_version, granted_at,
			confirmed_at, confirmed_via, confirmation_sent_at, renewal_requested_at, ended_at, ended_reason, ended_via)
		VALUES ($1, $2, $3, $4, $5, 'guest_apply', 'davet-v1', $6, $7, $8, $9, $10, $11, $12, $13)`,
		id, purpose, g.user, email, hmac, granted, g.confirmed, confirmedVia, g.sent, g.renewalRequested, g.ended, endedReason, endedVia)
	return id
}

func at(d time.Duration) *time.Time { v := testNow.Add(-d); return &v }

// A guest whose address holds an active invitation consent keeps their name
// and address; a pending, ended, other-purpose or account consent keeps
// nothing.
func TestGuestIdentityKeepsActiveInvitationConsentHolders(t *testing.T) {
	pool := migrated(t)
	ctx := context.Background()
	eventID := uuid.New()
	exec(t, pool, `INSERT INTO events (id, name, location, owner_team, end_date) VALUES ($1, 'old', 'YTÜ', 'SKY LAB', $2)`, eventID, testNow.Add(-800*day))
	guests := map[string]uuid.UUID{}
	for _, email := range []string{"active@example.com", "pending@example.com", "ended@example.com", "recruit@example.com", "account@example.com", "none@example.com"} {
		id := uuid.New()
		guests[email] = id
		exec(t, pool, `INSERT INTO tickets (id, event_id, ticket_type, guest_first_name, guest_email) VALUES ($1, $2, 'GUEST', 'Guest', $3)`, id, eventID, email)
	}
	member := uuid.New()
	exec(t, pool, `INSERT INTO users (id, email) VALUES ($1, 'account@example.com')`, member)
	insertGrant(t, pool, grant{email: "active@example.com", confirmed: at(day)})
	insertGrant(t, pool, grant{email: "pending@example.com", sent: at(day)})
	insertGrant(t, pool, grant{email: "ended@example.com", confirmed: at(10 * day), ended: at(day)})
	insertGrant(t, pool, grant{purpose: "recruitment_pool", email: "recruit@example.com", confirmed: at(day)})
	insertGrant(t, pool, grant{user: &member, confirmed: at(day)})

	now := testNow
	report, err := sweeper(pool, dryRunConfig(), &now, nil).Run(ctx, retention.RunOptions{Mode: retention.ModeApply, Trigger: retention.TriggerCLI, Rule: "guest_identity"})
	if err != nil || report.Status != retention.RunOK || len(report.Rules) != 1 || report.Rules[0].Changed != 5 {
		t.Fatalf("%v %+v", err, report)
	}
	for email, id := range guests {
		kept := scalar[string](t, pool, `SELECT guest_email FROM tickets WHERE id = $1`, id) != ""
		if kept != (email == "active@example.com") {
			t.Errorf("%s kept %v", email, kept)
		}
	}
	if full := scalar[bool](t, pool, `SELECT full_run FROM retention_runs`); full {
		t.Fatal("a one-rule run counted as a full run")
	}
}

// The contact consent lifecycle's destruction (docs/contact-consents.md): a
// grant that was never confirmed (pending, or ended as superseded or
// withdrawn before it was) goes 30 days after its last confirmation mail: it
// was never consent, so it is no proof. A renewal question unanswered for 60
// days (no renewal, no check-in since) ends the grant as expired and clears
// its address. A grant that was once confirmed and ended is proof, kept three
// years. A second run changes nothing.
func TestConsentRulesFollowTheConsentLifecycle(t *testing.T) {
	pool := migrated(t)
	ctx := context.Background()
	member := uuid.New()
	exec(t, pool, `INSERT INTO users (id, email) VALUES ($1, 'member@example.com')`, member)

	pendingOld := insertGrant(t, pool, grant{email: "pending.old@example.com", granted: at(40 * day), sent: at(40 * day)})
	pendingResent := insertGrant(t, pool, grant{email: "pending.resent@example.com", granted: at(40 * day), sent: at(10 * day)})
	pendingUnmailed := insertGrant(t, pool, grant{email: "pending.unmailed@example.com", granted: at(31 * day)})
	pendingFresh := insertGrant(t, pool, grant{email: "pending.fresh@example.com", granted: at(5 * day), sent: at(5 * day)})
	// A pending grant a verified one replaced was never consent: it goes on
	// the pending schedule, not after three years.
	supersededOld := insertGrant(t, pool, grant{email: "x", granted: at(40 * day), sent: at(40 * day), ended: at(35 * day), endedReason: "superseded"})
	supersededYoung := insertGrant(t, pool, grant{email: "x", granted: at(12 * day), sent: at(10 * day), ended: at(9 * day), endedReason: "superseded"})
	supersededAncient := insertGrant(t, pool, grant{email: "x", granted: at(4 * 365 * day), sent: at(4 * 365 * day), ended: at(4*365*day - day), endedReason: "superseded"})
	// Withdrawn before it was ever confirmed: no proof either.
	withdrawnPendingOld := insertGrant(t, pool, grant{email: "x", granted: at(40 * day), sent: at(40 * day), ended: at(39 * day)})
	withdrawnPendingYoung := insertGrant(t, pool, grant{email: "x", granted: at(12 * day), sent: at(12 * day), ended: at(11 * day)})
	withdrawnPendingAncient := insertGrant(t, pool, grant{email: "x", granted: at(4 * 365 * day), sent: at(4 * 365 * day), ended: at(4 * 365 * day)})

	unanswered := insertGrant(t, pool, grant{email: "unanswered@example.com", confirmed: at(4 * 365 * day), renewalRequested: at(70 * day)})
	attended := insertGrant(t, pool, grant{email: "attended@example.com", confirmed: at(4 * 365 * day), renewalRequested: at(70 * day)})
	asked := insertGrant(t, pool, grant{email: "asked@example.com", confirmed: at(4 * 365 * day), renewalRequested: at(30 * day)})
	account := insertGrant(t, pool, grant{user: &member, confirmed: at(4 * 365 * day), renewalRequested: at(70 * day)})

	proofOld := insertGrant(t, pool, grant{email: "x", confirmed: at(5 * 365 * day), ended: at(3*365*day + day)})
	proofYoung := insertGrant(t, pool, grant{email: "x", confirmed: at(5 * 365 * day), ended: at(2 * 365 * day), endedReason: "expired"})

	// The attended guest checked in after the question: that answers it.
	eventID, dayID, sessionID, ticketID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	exec(t, pool, `INSERT INTO events (id, name, location, owner_team, end_date) VALUES ($1, 'e', 'YTÜ', 'SKY LAB', $2)`, eventID, testNow.Add(-20*day))
	exec(t, pool, `INSERT INTO event_days (id, event_id, name) VALUES ($1, $2, 'Gün')`, dayID, eventID)
	exec(t, pool, `INSERT INTO sessions (id, event_day_id, title, session_type) VALUES ($1, $2, 'Açılış', 'TALK')`, sessionID, dayID)
	exec(t, pool, `INSERT INTO tickets (id, event_id, ticket_type, guest_email) VALUES ($1, $2, 'GUEST', 'attended@example.com')`, ticketID, eventID)
	exec(t, pool, `INSERT INTO ticket_checkins (id, ticket_id, event_day_id, session_id, created_at) VALUES ($1, $2, $3, $4, $5)`,
		uuid.New(), ticketID, dayID, sessionID, testNow.Add(-20*day))

	now := testNow
	s := sweeper(pool, dryRunConfig(), &now, nil)
	// A never-confirmed row is no proof: the proof rule leaves it to the
	// pending one, even past three years. It takes only the old confirmed one.
	proof, err := s.Run(ctx, retention.RunOptions{Mode: retention.ModeApply, Trigger: retention.TriggerCLI, Rule: "consent_proof"})
	if err != nil || len(proof.Rules) != 1 || proof.Rules[0].Changed != 1 {
		t.Fatalf("proof only: %v %+v", err, proof)
	}
	now = testNow.Add(time.Minute)
	report, err := s.Run(ctx, retention.RunOptions{Mode: retention.ModeApply, Trigger: retention.TriggerSchedule})
	if err != nil || report.Status != retention.RunOK {
		t.Fatalf("%v %+v", err, report)
	}
	got := results(report)
	for name, changed := range map[string]int64{"consent_pending": 6, "consent_renewal_unanswered": 2, "consent_proof": 0} {
		if r := got[name]; r.Changed != changed || r.Matched != changed || r.Overdue != 0 || r.Status != retention.RuleOK {
			t.Errorf("%s: %+v", name, r)
		}
	}
	state := func(id uuid.UUID) string {
		return scalar[string](t, pool, `SELECT COALESCE((SELECT concat_ws('|', COALESCE(email, '-'), COALESCE(ended_reason, 'open'), COALESCE(ended_via, '-'))
			FROM contact_consents WHERE id = $1), 'deleted')`, id)
	}
	for id, want := range map[uuid.UUID]string{
		pendingOld:        "deleted",
		pendingUnmailed:   "deleted",
		pendingResent:     "pending.resent@example.com|open|-",
		pendingFresh:      "pending.fresh@example.com|open|-",
		unanswered:        "-|expired|renewal_unanswered",
		account:           "-|expired|renewal_unanswered",
		attended:          "attended@example.com|open|-",
		asked:             "asked@example.com|open|-",
		proofOld:          "deleted",
		proofYoung:        "-|expired|link",
		supersededOld:     "deleted",
		supersededYoung:   "-|superseded|service",
		supersededAncient: "deleted",
		// Withdrawn while pending.
		withdrawnPendingOld:     "deleted",
		withdrawnPendingYoung:   "-|withdrawn|link",
		withdrawnPendingAncient: "deleted",
	} {
		if got := state(id); got != want {
			t.Errorf("grant %s: %q, want %q", id, got, want)
		}
	}
	now = testNow.Add(2 * time.Minute)
	again, err := s.Run(ctx, retention.RunOptions{Mode: retention.ModeApply, Trigger: retention.TriggerSchedule})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range again.Rules {
		if r.Rule.Kind == retention.KindSweep && r.Changed != 0 {
			t.Errorf("second run %s changed %d", r.Rule.Name, r.Changed)
		}
	}
}

// The brake refuses a change of more than a fifth of a table (or 50,000
// rows): apply changes nothing, alarms, and the run is partial; the dry run
// says it would. --allow-large on the command line works through it, in
// batches.
func TestBrakeRefusesALargeChangeUntilAllowed(t *testing.T) {
	pool := migrated(t)
	ctx := context.Background()
	urlID := uuid.New()
	exec(t, pool, `INSERT INTO urls (id, alias, url) VALUES ($1, 'many', 'https://forms.example/f')`, urlID)
	exec(t, pool, `INSERT INTO url_hits (id, url_id, alias, at, ip, user_agent)
		SELECT gen_random_uuid(), $1, 'many', $2, '203.0.113.' || (i % 250), 'Mozilla/5.0 (Fixture)' FROM generate_series(1, 1201) i`,
		urlID, testNow.Add(-400*day))
	now := testNow
	s := sweeper(pool, dryRunConfig(), &now, nil)
	only := func(mode retention.Mode, trigger retention.Trigger, allow bool) retention.RuleResult {
		t.Helper()
		report, err := s.Run(ctx, retention.RunOptions{Mode: mode, Trigger: trigger, Rule: "url_hits_scrub", AllowLarge: allow})
		if err != nil || len(report.Rules) != 1 {
			t.Fatalf("%v %+v", err, report)
		}
		return report.Rules[0]
	}
	if r := only(retention.ModeDryRun, retention.TriggerSchedule, false); r.Status != retention.RuleWouldRefuseLarge || r.Matched != 1201 || *r.TableRows != 1201 {
		t.Fatalf("dry run %+v", r)
	}
	if r := only(retention.ModeApply, retention.TriggerSchedule, false); r.Status != retention.RuleRefusedLarge || r.Changed != 0 || r.Overdue != 1201 {
		t.Fatalf("apply %+v", r)
	}
	if got := scalar[int](t, pool, `SELECT count(*) FROM url_hits WHERE ip <> ''`); got != 1201 {
		t.Fatalf("the refused rule changed rows: %d left", got)
	}
	if got := scalar[string](t, pool, `SELECT status FROM retention_runs WHERE mode = 'apply'`); got != "partial" {
		t.Fatalf("refused run %q", got)
	}
	if _, err := s.Run(ctx, retention.RunOptions{Mode: retention.ModeApply, Trigger: retention.TriggerSchedule, AllowLarge: true}); err == nil {
		t.Fatal("the schedule passed --allow-large")
	}
	if _, err := s.Run(ctx, retention.RunOptions{Mode: retention.ModeDryRun, Trigger: retention.TriggerCLI, AllowLarge: true}); err == nil {
		t.Fatal("a dry run passed --allow-large")
	}
	if r := only(retention.ModeApply, retention.TriggerCLI, true); r.Status != retention.RuleOK || r.Changed != 1201 || r.Overdue != 0 {
		t.Fatalf("allowed %+v", r)
	}
	if got := scalar[int](t, pool, `SELECT count(*) FROM url_hits WHERE ip <> '' OR user_agent <> ''`); got != 0 {
		t.Fatalf("%d rows left after an allowed apply", got)
	}
	if got := scalar[bool](t, pool, `SELECT allow_large FROM retention_runs WHERE triggered_by = 'cli'`); !got {
		t.Fatal("allow_large not recorded")
	}
}

// One run at a time across replicas: a run that finds the lock taken
// records that it skipped and changes nothing; a run left running by a
// process that went away is marked abandoned by the next one.
func TestOneRunAtATimeAndAbandonedRuns(t *testing.T) {
	pool := migrated(t)
	ctx := context.Background()
	now := testNow
	s := sweeper(pool, dryRunConfig(), &now, nil)

	holder, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var locked bool
	if err := holder.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended('core-backend retention sweep', 0))`).Scan(&locked); err != nil || !locked {
		t.Fatalf("lock %v %v", locked, err)
	}
	report, err := s.Run(ctx, retention.RunOptions{Mode: retention.ModeApply, Trigger: retention.TriggerSchedule})
	if !errors.Is(err, retention.ErrLocked) || report.Status != retention.RunSkippedLocked || len(report.Rules) != 0 {
		t.Fatalf("%v %+v", err, report)
	}
	if got := scalar[string](t, pool, `SELECT status || ':' || (period_id IS NULL)::text FROM retention_runs`); got != "skipped_locked:true" {
		t.Fatalf("skipped run %q", got)
	}
	if _, err := holder.Exec(ctx, `SELECT pg_advisory_unlock_all()`); err != nil {
		t.Fatal(err)
	}
	holder.Release()

	stale := uuid.New()
	exec(t, pool, `INSERT INTO retention_runs (id, mode, triggered_by, full_run, rule_set_version, started_at, status)
		VALUES ($1, 'apply', 'schedule', true, 1, $2, 'running')`, stale, testNow.Add(-time.Hour))
	if report, err := s.Run(ctx, retention.RunOptions{Mode: retention.ModeDryRun, Trigger: retention.TriggerSchedule}); err != nil || report.Status != retention.RunOK {
		t.Fatalf("%v %+v", err, report)
	}
	if got := scalar[string](t, pool, `SELECT status || ':' || error_code FROM retention_runs WHERE id = $1`, stale); got != "abandoned:abandoned" {
		t.Fatalf("stale run %q", got)
	}
	// The lock went with the run's connection: the next run takes it.
	if _, err := s.Run(ctx, retention.RunOptions{Mode: retention.ModeDryRun, Trigger: retention.TriggerSchedule}); err != nil {
		t.Fatal(err)
	}
}

// The schedule and the periods read the database: a restarted core (a new
// sweeper) does not run again before 23 hours, a period closes with its
// counts at the first run after its end, a period without a run is closed
// as such, and the metrics say so.
func TestScheduleAndPeriodsFollowTheRecords(t *testing.T) {
	pool := migrated(t)
	ctx := context.Background()
	config := retention.Config{Mode: retention.ModeApply, Period: 10 * day, MediaRecoveryWindow: 30 * day}
	now := testNow
	s := sweeper(pool, config, &now, nil)
	attention := []retention.Attention{}
	metrics := retention.NewMetrics(pool, config, func(a retention.Attention) { attention = append(attention, a) })
	metrics.SetClock(func() time.Time { return now })

	if due, err := s.Due(ctx, retention.ModeApply); err != nil || !due {
		t.Fatalf("first run due %v %v", due, err)
	}
	if err := metrics.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if text := metrics.Prometheus(); !strings.Contains(text, "skylab_retention_attention 0\n") || strings.Contains(text, "last_success") {
		t.Fatalf("before any run:\n%s", text)
	}
	if _, err := s.Run(ctx, retention.RunOptions{Mode: retention.ModeApply, Trigger: retention.TriggerSchedule}); err != nil {
		t.Fatal(err)
	}
	firstPeriod := scalar[uuid.UUID](t, pool, `SELECT id FROM retention_periods WHERE closed_at IS NULL AND started_at = $1 AND ends_at = $2`, testNow, testNow.Add(10*day))

	restarted := sweeper(pool, config, &now, nil)
	for _, c := range []struct {
		after time.Duration
		mode  retention.Mode
		due   bool
	}{
		{22 * time.Hour, retention.ModeApply, false},
		{23 * time.Hour, retention.ModeApply, true},
		{time.Hour, retention.ModeDryRun, true},
	} {
		now = testNow.Add(c.after)
		if due, err := restarted.Due(ctx, c.mode); err != nil || due != c.due {
			t.Fatalf("%s %s: due %v %v", c.mode, c.after, due, err)
		}
	}
	// A replica that found the run due while another ran it checks again
	// once it holds the lock, and does nothing.
	now = testNow.Add(time.Hour)
	if report, err := restarted.Run(ctx, retention.RunOptions{Mode: retention.ModeApply, Trigger: retention.TriggerSchedule, OnlyIfDue: true}); !errors.Is(err, retention.ErrNotDue) || report.RunID != uuid.Nil {
		t.Fatalf("not due: %v %+v", err, report)
	}
	if got := scalar[int](t, pool, `SELECT count(*) FROM retention_runs`); got != 1 {
		t.Fatalf("a run that was not due left %d records", got)
	}
	// A one-rule run is not the day's run.
	now = testNow.Add(24 * time.Hour)
	if _, err := restarted.Run(ctx, retention.RunOptions{Mode: retention.ModeApply, Trigger: retention.TriggerCLI, Rule: "door_staff"}); err != nil {
		t.Fatal(err)
	}
	if due, _ := restarted.Due(ctx, retention.ModeApply); !due {
		t.Fatal("a one-rule run made the full run not due")
	}
	if _, err := restarted.Run(ctx, retention.RunOptions{Mode: retention.ModeDryRun, Trigger: retention.TriggerCLI}); err != nil {
		t.Fatal(err)
	}

	// 25 days on: the first period closes with its two full runs, the
	// second (days 10 to 20) closes with none, the third is open.
	now = testNow.Add(25 * day)
	report, err := restarted.Run(ctx, retention.RunOptions{Mode: retention.ModeApply, Trigger: retention.TriggerSchedule})
	if err != nil || len(report.ClosedPeriods) != 2 {
		t.Fatalf("%v %+v", err, report.ClosedPeriods)
	}
	// Both were apply periods: the first had a scheduled apply run, the
	// second no scheduled run, and core was in apply mode when it closed.
	if p := report.ClosedPeriods[0]; p.ID != firstPeriod || p.Mode != retention.ModeApply || p.ApplyRuns != 1 || p.DryRuns != 1 || !p.StartedAt.Equal(testNow) {
		t.Fatalf("first period %+v", p)
	}
	if p := report.ClosedPeriods[1]; p.Mode != retention.ModeApply || p.ApplyRuns != 0 || p.DryRuns != 0 || !p.StartedAt.Equal(testNow.Add(10*day)) || !p.EndsAt.Equal(testNow.Add(20*day)) {
		t.Fatalf("second period %+v", p)
	}
	if got := scalar[string](t, pool, `SELECT started_at::text || '/' || ends_at::text FROM retention_periods WHERE closed_at IS NULL`); got != fmt.Sprint(testNow.Add(20*day).Format("2006-01-02 15:04:05+00"), "/", testNow.Add(30*day).Format("2006-01-02 15:04:05+00")) {
		t.Fatalf("open period %q", got)
	}

	if err := metrics.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	text := metrics.Prometheus()
	for _, want := range []string{
		`skylab_retention_mode{mode="apply"} 1`,
		`skylab_retention_mode{mode="off"} 0`,
		fmt.Sprintf(`skylab_retention_last_success_timestamp_seconds{mode="apply"} %d`, now.Unix()),
		fmt.Sprintf(`skylab_retention_last_success_timestamp_seconds{mode="dry-run"} %d`, testNow.Add(24*time.Hour).Unix()),
		fmt.Sprintf("skylab_retention_period_seconds_left %d", int64(5*day/time.Second)),
		`skylab_retention_rows_matched{rule="guest_phone"} 0`,
		`skylab_retention_rule_status{rule="url_hits_age",status="not_applicable"} 1`,
		"skylab_retention_attention 1\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics lack %q:\n%s", want, text)
		}
	}
	if len(attention) != 1 || attention[0] != (retention.Attention{Reason: retention.AttentionPeriodWithoutRun}) {
		t.Fatalf("attention %+v", attention)
	}

	// Two days without a successful run in the configured mode alarm.
	now = testNow.Add(27*day + time.Hour)
	if err := metrics.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if got := metrics.Attention(); len(got) != 2 || got[0].Reason != retention.AttentionStale {
		t.Fatalf("stale attention %+v", got)
	}
}

// A period's success depends on its mode. In dry-run mode a successful dry
// run is the period's run, so the rollout raises no alarm, and switching to
// apply does not alarm on the dry-run period before it. A period in apply
// mode needs a successful apply run: dry runs alone (here beside a failed
// scheduled apply run) alarm, and the next period's apply run clears it.
func TestPeriodAccountingFollowsTheMode(t *testing.T) {
	pool := migrated(t)
	ctx := context.Background()
	dryConfig := retention.Config{Mode: retention.ModeDryRun, Period: 10 * day, MediaRecoveryWindow: 30 * day}
	applyConfig := retention.Config{Mode: retention.ModeApply, Period: 10 * day, MediaRecoveryWindow: 30 * day}
	now := testNow
	dry, apply := sweeper(pool, dryConfig, &now, nil), sweeper(pool, applyConfig, &now, nil)
	attention := func(config retention.Config) string {
		t.Helper()
		metrics := retention.NewMetrics(pool, config, func(retention.Attention) {})
		metrics.SetClock(func() time.Time { return now })
		if err := metrics.Refresh(ctx); err != nil {
			t.Fatal(err)
		}
		return fmt.Sprint(metrics.Attention())
	}
	run := func(s *retention.Sweeper, mode retention.Mode, trigger retention.Trigger) retention.Report {
		t.Helper()
		report, err := s.Run(ctx, retention.RunOptions{Mode: mode, Trigger: trigger})
		if err != nil {
			t.Fatal(err)
		}
		return report
	}

	// Rollout: a period of scheduled dry runs is a dry-run period, met by its
	// dry run, even when apply is switched on before it closes (the closing
	// run is the first scheduled apply run).
	run(dry, retention.ModeDryRun, retention.TriggerSchedule)
	now = testNow.Add(10*day + time.Hour)
	report := run(apply, retention.ModeApply, retention.TriggerSchedule)
	if len(report.ClosedPeriods) != 1 || report.ClosedPeriods[0].Mode != retention.ModeDryRun || report.ClosedPeriods[0].DryRuns != 1 || report.ClosedPeriods[0].ApplyRuns != 0 {
		t.Fatalf("dry-run period %+v", report.ClosedPeriods)
	}
	for _, config := range []retention.Config{dryConfig, applyConfig} {
		if got := attention(config); strings.Contains(got, retention.AttentionPeriodWithoutRun) {
			t.Fatalf("%s: a dry-run period with its dry run alarms: %s", config.Mode, got)
		}
	}
	// The next period had that scheduled apply run: an apply period, met.
	now = testNow.Add(20*day + time.Hour)
	report = run(apply, retention.ModeDryRun, retention.TriggerCLI)
	if len(report.ClosedPeriods) != 1 || report.ClosedPeriods[0].Mode != retention.ModeApply || report.ClosedPeriods[0].ApplyRuns != 1 {
		t.Fatalf("first apply period %+v", report.ClosedPeriods)
	}

	// Then the scheduled apply run fails (a record as a crash leaves it) and
	// only dry runs succeed in the period: an apply period, not met.
	period := scalar[uuid.UUID](t, pool, `SELECT id FROM retention_periods WHERE closed_at IS NULL`)
	exec(t, pool, `INSERT INTO retention_runs (id, period_id, mode, triggered_by, full_run, rule_set_version, started_at, finished_at, status, error_code)
		VALUES ($1, $2, 'apply', 'schedule', true, 1, $3, $3, 'failed', 'sqlstate_57014')`, uuid.New(), period, testNow.Add(22*day))
	now = testNow.Add(23 * day)
	run(apply, retention.ModeDryRun, retention.TriggerCLI)
	now = testNow.Add(30*day + time.Hour)
	report = run(apply, retention.ModeDryRun, retention.TriggerCLI)
	if len(report.ClosedPeriods) != 1 || report.ClosedPeriods[0].Mode != retention.ModeApply || report.ClosedPeriods[0].ApplyRuns != 0 || report.ClosedPeriods[0].DryRuns != 2 {
		t.Fatalf("apply period with dry runs only %+v", report.ClosedPeriods)
	}
	if got := attention(applyConfig); !strings.Contains(got, "{"+retention.AttentionPeriodWithoutRun+" }") {
		t.Fatalf("an apply period with dry runs only: %s", got)
	}
	if got := scalar[string](t, pool, `SELECT string_agg(mode || ':' || apply_runs || ':' || dry_runs, ',' ORDER BY started_at) FROM retention_periods WHERE closed_at IS NOT NULL`); got != "dry-run:0:1,apply:1:0,apply:0:2" {
		t.Fatalf("periods %q", got)
	}

	// The next period has its scheduled apply run: the alarm clears.
	now = testNow.Add(31 * day)
	run(apply, retention.ModeApply, retention.TriggerSchedule)
	now = testNow.Add(40*day + time.Hour)
	report = run(apply, retention.ModeDryRun, retention.TriggerCLI)
	if len(report.ClosedPeriods) != 1 || report.ClosedPeriods[0].Mode != retention.ModeApply || report.ClosedPeriods[0].ApplyRuns != 1 {
		t.Fatalf("next apply period %+v", report.ClosedPeriods)
	}
	if got := attention(applyConfig); strings.Contains(got, retention.AttentionPeriodWithoutRun) {
		t.Fatalf("still alarming: %s", got)
	}
}

// Metrics and attention come from the latest run's records: a refused rule,
// a failed rule, rows overdue after apply, and an audit whose cleanup left
// rows each need a person; an audit of Media still in use does not.
func TestMetricsAlarmOnWhatNeedsAPerson(t *testing.T) {
	pool := migrated(t)
	ctx := context.Background()
	seed(t, pool)
	now := testNow
	config := dryRunConfig()
	s := sweeper(pool, config, &now, nil)
	metrics := retention.NewMetrics(pool, config, func(retention.Attention) {})
	metrics.SetClock(func() time.Time { return now })
	if text := metrics.Prometheus(); text != "" {
		t.Fatalf("metrics before the first read: %q", text)
	}
	if _, err := s.Run(ctx, retention.RunOptions{Mode: retention.ModeDryRun, Trigger: retention.TriggerSchedule}); err != nil {
		t.Fatal(err)
	}
	if err := metrics.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	// A dry run's due rows are no alarm; the hourly cleanups' leftovers are.
	want := []retention.Attention{
		{Reason: retention.AttentionOverdue, Rule: "mail_snapshots_age"},
		{Reason: retention.AttentionOverdue, Rule: "read_link_opens_age"},
		{Reason: retention.AttentionOverdue, Rule: "read_links_age"},
		{Reason: retention.AttentionOverdue, Rule: "url_hits_age"},
	}
	if got := metrics.Attention(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("attention %+v", got)
	}
	text := metrics.Prometheus()
	for _, value := range personal {
		if strings.Contains(text, value) {
			t.Fatalf("%q in metrics", value)
		}
	}
	if !strings.Contains(text, `skylab_retention_rows_matched{rule="guest_phone"} 9`) || !strings.Contains(text, `skylab_retention_rule_status{rule="guest_phone",status="dry_run"} 1`) {
		t.Fatalf("metrics:\n%s", text)
	}
	if strings.Contains(text, "skylab_retention_rows_changed_total") {
		t.Fatalf("a dry run counted as changes:\n%s", text)
	}

	// A rule that fails (here: its table is gone) is recorded with its
	// SQLSTATE and alarms; the run goes on and ends partial.
	exec(t, pool, `ALTER TABLE event_door_staff RENAME TO event_door_staff_gone`)
	now = testNow.Add(time.Minute)
	report, err := s.Run(ctx, retention.RunOptions{Mode: retention.ModeApply, Trigger: retention.TriggerSchedule})
	if err != nil || report.Status != retention.RunPartial {
		t.Fatalf("%v %+v", err, report)
	}
	if r := results(report)["door_staff"]; r.Status != retention.RuleFailed || r.ErrorCode != "sqlstate_42p01" {
		t.Fatalf("door_staff %+v", r)
	}
	if r := results(report)["guest_phone"]; r.Status != retention.RuleOK || r.Changed != 9 {
		t.Fatalf("the run stopped at the failed rule: %+v", r)
	}
	if err := metrics.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	got := fmt.Sprint(metrics.Attention())
	if !strings.Contains(got, "{rule_failed door_staff}") {
		t.Fatalf("attention %s", got)
	}
	text = metrics.Prometheus()
	for _, want := range []string{
		`skylab_retention_rows_changed_total{rule="guest_identity",table="certificates"} 1`,
		`skylab_retention_rows_changed_total{rule="guest_identity",table="tickets"} 4`,
		`skylab_retention_rows_changed_total{rule="guest_phone",table="tickets"} 9`,
		`skylab_retention_rule_failures_total{rule="door_staff"} 1`,
		`skylab_retention_refused_large{rule="guest_phone"} 0`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics lack %q:\n%s", want, text)
		}
	}
}

// Maintain starts nothing with the mode off, and in a mode runs at once,
// then refreshes the metrics.
func TestMaintainRunsWhenDue(t *testing.T) {
	pool := migrated(t)
	ctx, cancel := context.WithCancel(context.Background())
	off := retention.NewSweeper(pool, retention.Config{Mode: retention.ModeOff, Period: 90 * day}, nil)
	<-retention.Maintain(ctx, off, nil, time.Hour, func(err error) { t.Error(err) })
	if got := scalar[int](t, pool, `SELECT count(*) FROM retention_runs`); got != 0 {
		t.Fatalf("off ran %d runs", got)
	}

	config := dryRunConfig()
	s := retention.NewSweeper(pool, config, nil)
	metrics := retention.NewMetrics(pool, config, func(retention.Attention) {})
	done := retention.Maintain(ctx, s, metrics, time.Hour, func(err error) { t.Error(err) })
	deadline := time.Now().Add(30 * time.Second)
	for metrics.Prometheus() == "" {
		if time.Now().After(deadline) {
			t.Fatal("no metrics after the first check")
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	<-done
	if got := scalar[string](t, pool, `SELECT string_agg(mode || ':' || triggered_by || ':' || status, ',') FROM retention_runs`); got != "dry-run:schedule:ok" {
		t.Fatalf("runs %q", got)
	}
}
