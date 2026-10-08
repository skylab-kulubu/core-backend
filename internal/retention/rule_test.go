package retention

import (
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/skylab-kulubu/core-backend/internal/consent"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/shorturl"
)

func env(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func TestParseModeAcceptsOnlyTheThreeModes(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]Mode{"": ModeOff, " off ": ModeOff, "dry-run": ModeDryRun, "apply": ModeApply} {
		got, err := ParseMode(raw)
		if err != nil || got != want {
			t.Fatalf("%q: %q %v", raw, got, err)
		}
	}
	for _, bad := range []string{"dry_run", "dryrun", "APPLY", "on", "true", "1"} {
		_, err := ParseMode(bad)
		if err == nil || err.Error() != "RETENTION_SWEEP_MODE must be off, dry-run or apply" {
			t.Fatalf("%q: %v", bad, err)
		}
	}
}

// Off by default and harmless: with the mode off nothing else is read, so a
// bad interval cannot stop a release that leaves the sweep off.
func TestConfigReadsThePeriodOnlyWhenTheSweepIsOn(t *testing.T) {
	t.Parallel()
	config, err := ConfigFromEnv(env(map[string]string{"PERIODIC_DESTRUCTION_INTERVAL": "nonsense"}), 0)
	if err != nil || config.Mode != ModeOff {
		t.Fatalf("off: %+v %v", config, err)
	}
	if config.MediaRecoveryWindow != media.DefaultBlobRecoveryWindow {
		t.Fatalf("media window %s", config.MediaRecoveryWindow)
	}
	_, err = ConfigFromEnv(env(map[string]string{ModeEnv: "dry-run", "PERIODIC_DESTRUCTION_INTERVAL": "nonsense"}), 0)
	if err == nil || !strings.HasPrefix(err.Error(), "PERIODIC_DESTRUCTION_INTERVAL ") || strings.Contains(err.Error(), "nonsense") {
		t.Fatalf("bad interval: %v", err)
	}
	config, err = ConfigFromEnv(env(map[string]string{ModeEnv: "apply", "PERIODIC_DESTRUCTION_INTERVAL": "2000h"}), 10*24*time.Hour)
	if err != nil || config.Mode != ModeApply || config.Period != 2000*time.Hour || config.MediaRecoveryWindow != 10*24*time.Hour {
		t.Fatalf("apply: %+v %v", config, err)
	}
	config, err = ConfigFromEnv(env(map[string]string{ModeEnv: "dry-run"}), 0)
	if err != nil || config.Period != 90*24*time.Hour {
		t.Fatalf("default period: %+v %v", config, err)
	}
	if _, err := ConfigFromEnv(env(map[string]string{ModeEnv: "yes"}), 0); err == nil {
		t.Fatal("bad mode accepted")
	}
}

// The hourly cleanups keep today's windows until apply: only then does
// something empty a click's or an open's address after a year (ADR-0062).
func TestHourlyCleanupWindowsChangeOnlyInApply(t *testing.T) {
	t.Parallel()
	for _, mode := range []Mode{ModeOff, ModeDryRun} {
		if !HourlyHitDeletion(mode) || ReadLinkWindow(mode) != media.ReadLinkRetention {
			t.Fatalf("%s: hit deletion %v, read link window %s", mode, HourlyHitDeletion(mode), ReadLinkWindow(mode))
		}
	}
	if HourlyHitDeletion(ModeApply) || ReadLinkWindow(ModeApply) != 3*365*24*time.Hour {
		t.Fatalf("apply: hit deletion %v, read link window %s", HourlyHitDeletion(ModeApply), ReadLinkWindow(ModeApply))
	}
	if hitDeletionWindow != shorturl.HitRetention || shorturl.HitRetention != 90*24*time.Hour {
		t.Fatalf("hit deletion window %s", hitDeletionWindow)
	}
}

// The brake refuses more than 50,000 rows, and more than a fifth of a table
// once a run would change more than 1,000 rows. Below that the share is
// left alone: one large Event's guests (401 phones of 2,000 Tickets) or a
// young consent table's first expiries are a normal day's work.
func TestBrakeRefusesAFifthOfATableOrFiftyThousandRows(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		matched, table int64
		refuse         bool
	}{
		{0, 0, false},
		{401, 2000, false},  // one large Event's phones
		{150, 300, false},   // a young consent table
		{1000, 1000, false}, // the whole of a small table: the share is not checked
		{1001, 5005, false}, // exactly a fifth
		{1002, 5005, true},  // more than a fifth
		{1500, 2500, true},
		{1001, 10000, false}, // a small share
		{50000, 10_000_000, false},
		{50001, 10_000_000, true},
		{2001, 10000, true},
	} {
		if got := brakeRefuses(c.matched, c.table); got != c.refuse {
			t.Fatalf("matched %d of %d: refuse %v", c.matched, c.table, got)
		}
	}
}

var identifier = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

func allRules() [][]Rule {
	var sets [][]Rule
	for _, mode := range []Mode{ModeOff, ModeDryRun, ModeApply} {
		for _, consents := range []bool{false, true} {
			sets = append(sets, Rules(Config{Mode: mode, Period: 90 * day, MediaRecoveryWindow: 30 * day}, Schema{ContactConsents: consents}))
		}
	}
	return sets
}

func TestRulesAreWellFormed(t *testing.T) {
	t.Parallel()
	for _, rules := range allRules() {
		seen := map[string]bool{}
		for _, rule := range rules {
			if !identifier.MatchString(rule.Name) || !identifier.MatchString(rule.Table) || rule.Version < 1 || seen[rule.Name] {
				t.Fatalf("rule %+v", rule)
			}
			seen[rule.Name] = true
			if rule.Period <= 0 || rule.where == "" || rule.alias == "" {
				t.Fatalf("%s: period %s, where %q", rule.Name, rule.Period, rule.where)
			}
			if rule.RelatedTable != "" && (!identifier.MatchString(rule.RelatedTable) || rule.related == "") {
				t.Fatalf("%s: related table %q", rule.Name, rule.RelatedTable)
			}
			switch rule.Kind {
			case KindSweep:
				if rule.key == "" || rule.applySQL() == "" || (rule.Action == ActionScrub) != (rule.set != "") ||
					(rule.Action != ActionScrub && rule.Action != ActionDelete) || rule.NotApplicable != "" || rule.Alarm {
					t.Fatalf("sweep rule %s malformed", rule.Name)
				}
			case KindAudit:
				if rule.Action != ActionCount || rule.applySQL() != "" || rule.set != "" || rule.related != "" {
					t.Fatalf("audit %s changes something", rule.Name)
				}
			default:
				t.Fatalf("%s: kind %q", rule.Name, rule.Kind)
			}
		}
	}
}

// The periods are the policy's (ADR-0062); changing one is a new version
// and a change to the policy text.
func TestRulePeriodsAreThePolicy(t *testing.T) {
	t.Parallel()
	want := map[string]struct {
		period  time.Duration
		action  Action
		table   string
		related string
		version int
	}{
		"guest_phone":    {90 * day, ActionScrub, "tickets", "", 1},
		"guest_identity": {730 * day, ActionScrub, "tickets", "certificates", 3},
		"door_staff":     {90 * day, ActionDelete, "event_door_staff", "", 1},
		"url_hits_scrub": {365 * day, ActionScrub, "url_hits", "", 3},
		"read_link_ip":   {365 * day, ActionScrub, "media_read_link_opens", "", 1},
	}
	sweeps := 0
	for _, rule := range Rules(Config{Mode: ModeApply, Period: 90 * day, MediaRecoveryWindow: 30 * day}, Schema{}) {
		if rule.Kind != KindSweep {
			continue
		}
		sweeps++
		w, ok := want[rule.Name]
		if !ok || rule.Period != w.period || rule.Action != w.action || rule.Table != w.table || rule.RelatedTable != w.related || rule.Version != w.version {
			t.Fatalf("%s: %s %s %s %s v%d", rule.Name, rule.Period, rule.Action, rule.Table, rule.RelatedTable, rule.Version)
		}
	}
	if sweeps != len(want) || RuleSetVersion != 3 {
		t.Fatalf("%d sweep rules, set v%d", sweeps, RuleSetVersion)
	}
}

func TestAuditsFollowTheHourlyCleanupWindows(t *testing.T) {
	t.Parallel()
	periods := func(mode Mode) map[string]Rule {
		out := map[string]Rule{}
		for _, rule := range Rules(Config{Mode: mode, Period: 90 * day, MediaRecoveryWindow: 30 * day}, Schema{}) {
			if rule.Kind == KindAudit {
				out[rule.Name] = rule
			}
		}
		return out
	}
	dry := periods(ModeDryRun)
	if dry["url_hits_age"].Period != 91*day || dry["url_hits_age"].NotApplicable != "" ||
		dry["read_links_age"].Period != 366*day || dry["read_link_opens_age"].Period != 366*day ||
		dry["mail_snapshots_age"].Period != day || dry["media_archived_objects"].Period != 31*day ||
		dry["media_expired_objects"].Period != day {
		t.Fatalf("dry-run audits %+v", dry)
	}
	apply := periods(ModeApply)
	if apply["url_hits_age"].NotApplicable == "" || apply["read_links_age"].Period != (3*365+1)*day {
		t.Fatalf("apply audits %+v", apply)
	}
	for name, rule := range dry {
		alarm := !strings.HasPrefix(name, "media_")
		if rule.Alarm != alarm {
			t.Fatalf("%s alarm %v", name, rule.Alarm)
		}
	}
}

// The dry run and apply are generated from one where clause, so they cannot
// drift apart: apply's batch selects exactly the rows the count counts,
// and overdue is the same count at an earlier cutoff.
func TestDryRunAndApplyShareOneWhereClause(t *testing.T) {
	t.Parallel()
	for _, rules := range allRules() {
		for _, rule := range rules {
			if rule.countSQL() != "SELECT count(*) FROM "+rule.Table+" "+rule.alias+" WHERE "+rule.where {
				t.Fatalf("%s count %q", rule.Name, rule.countSQL())
			}
			if rule.Kind != KindSweep {
				continue
			}
			batch := "WITH batch AS (SELECT " + rule.alias + "." + rule.key + " AS k FROM " + rule.Table + " " + rule.alias +
				" WHERE " + rule.where + " LIMIT $2 FOR UPDATE OF " + rule.alias + " SKIP LOCKED)"
			if !strings.HasPrefix(rule.applySQL(), batch) || strings.Count(rule.applySQL(), rule.where) != 1 {
				t.Fatalf("%s apply %q", rule.Name, rule.applySQL())
			}
		}
	}
}

func TestGuestIdentityKeepsInvitationConsentHoldersOnceConsentsExist(t *testing.T) {
	t.Parallel()
	find := func(s Schema) Rule {
		for _, rule := range Rules(Config{Mode: ModeApply, Period: 90 * day}, s) {
			if rule.Name == "guest_identity" {
				return rule
			}
		}
		t.Fatal("no guest_identity")
		return Rule{}
	}
	if strings.Contains(find(Schema{}).where, "contact_consents") {
		t.Fatal("refers to contact_consents before it exists")
	}
	with := find(Schema{ContactConsents: true}).where
	if !strings.Contains(with, "AND NOT EXISTS (SELECT 1 FROM contact_consents gcc") ||
		!strings.Contains(with, "gcc.purpose = 'event_invitations'") || !strings.Contains(with, "gcc.confirmed_at IS NOT NULL") ||
		!strings.Contains(with, "gcc.ended_at IS NULL") {
		t.Fatalf("consent clause %q", with)
	}
	for _, rule := range Rules(Config{Mode: ModeApply}, Schema{ContactConsents: true}) {
		if rule.Name != "guest_identity" && strings.Contains(rule.where, "contact_consents") {
			t.Fatalf("%s refers to contact_consents", rule.Name)
		}
	}
}

// A record or a log line holds a code, never an error's message: a
// PostgreSQL message can quote the value that broke a statement.
func TestErrorCodesNeverCarryAMessage(t *testing.T) {
	t.Parallel()
	err := &pgconn.PgError{Code: "22P02", Message: `invalid input syntax for type uuid: "ada@example.com"`, Detail: "Key (guest_email)=(ada@example.com)"}
	if got := errorCode(errors.Join(errors.New("batch"), err)); got != "sqlstate_22p02" {
		t.Fatalf("code %q", got)
	}
	if got := errorCode(errors.New("ada@example.com")); got != "error" {
		t.Fatalf("code %q", got)
	}
	for _, code := range []string{errorCode(err), "timeout", "canceled", "abandoned"} {
		if !regexp.MustCompile(`^[a-z0-9_]{0,64}$`).MatchString(code) {
			t.Fatalf("code %q breaks the record's check", code)
		}
	}
}

// Staleness is two days without a success in the configured mode, measured
// so that a deploy never hides it and a switch of mode does not raise it
// before the new mode's first run.
func TestStaleSinceNeitherResetsOnDeployNorAlarmsOnASwitch(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) *time.Time { at := now.Add(-d); return &at }
	for name, c := range map[string]struct {
		lastSuccess         time.Time
		otherMode, firstRun *time.Time
		started             time.Time
		want                time.Time
	}{
		"no run recorded: since this process watches": {time.Time{}, nil, nil, now.Add(-time.Hour), now.Add(-time.Hour)},
		"steady, a deploy an hour ago":                {now.Add(-72 * time.Hour), nil, ago(30 * day), now.Add(-time.Hour), now.Add(-72 * time.Hour)},
		"never succeeded: since the first run":        {time.Time{}, nil, ago(50 * time.Hour), now, now.Add(-50 * time.Hour)},
		"switched from dry-run yesterday":             {now.Add(-90 * day), ago(20 * time.Hour), ago(200 * day), now, now.Add(-20 * time.Hour)},
		"switched long ago, failing since":            {now.Add(-5 * day), ago(40 * day), ago(200 * day), now, now.Add(-5 * day)},
	} {
		if got := staleSince(c.lastSuccess, c.otherMode, c.firstRun, c.started); !got.Equal(c.want) {
			t.Errorf("%s: %s, want %s", name, got, c.want)
		}
	}
}

// The contact consent rules follow the consent lifecycle
// (docs/contact-consents.md) and exist once contact_consents does.
func TestConsentRulesFollowTheConsentLifecycle(t *testing.T) {
	t.Parallel()
	want := map[string]struct {
		period time.Duration
		action Action
	}{
		"consent_pending":            {consent.PendingTTL, ActionDelete},
		"consent_renewal_unanswered": {consent.RenewalAnswerWindow, ActionScrub},
		"consent_proof":              {consent.ProofRetention, ActionDelete},
	}
	config := Config{Mode: ModeApply, Period: 90 * day, MediaRecoveryWindow: 30 * day}
	for _, rule := range Rules(config, Schema{}) {
		if rule.Table == "contact_consents" {
			t.Fatalf("%s without contact_consents", rule.Name)
		}
	}
	found := 0
	for _, rule := range Rules(config, Schema{ContactConsents: true}) {
		if rule.Table != "contact_consents" {
			continue
		}
		found++
		w, ok := want[rule.Name]
		if !ok || rule.Period != w.period || rule.Action != w.action || rule.Kind != KindSweep || rule.Version != 1 {
			t.Fatalf("%s: %s %s", rule.Name, rule.Period, rule.Action)
		}
	}
	if found != len(want) || consent.PendingTTL != 30*day || consent.RenewalAnswerWindow != 60*day || consent.ProofRetention != 3*365*day {
		t.Fatalf("%d consent rules", found)
	}
	names := RuleNames(config)
	if len(names) != 14 || names[5] != "consent_pending" {
		t.Fatalf("names %v", names)
	}
}
