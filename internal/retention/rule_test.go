package retention

import (
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
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

func TestBrakeRefusesAFifthOfATableOrFiftyThousandRows(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		matched, table int64
		refuse         bool
	}{
		{0, 0, false},
		{100, 100, false},   // a small table's handful never trips it
		{101, 505, false},   // exactly a fifth
		{102, 505, true},    // more than a fifth
		{101, 10000, false}, // a small share
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
	}{
		"guest_phone":    {90 * day, ActionScrub, "tickets", ""},
		"guest_identity": {730 * day, ActionScrub, "tickets", "certificates"},
		"door_staff":     {90 * day, ActionDelete, "event_door_staff", ""},
		"url_hits_scrub": {365 * day, ActionScrub, "url_hits", ""},
		"read_link_ip":   {365 * day, ActionScrub, "media_read_link_opens", ""},
	}
	sweeps := 0
	for _, rule := range Rules(Config{Mode: ModeApply, Period: 90 * day, MediaRecoveryWindow: 30 * day}, Schema{}) {
		if rule.Kind != KindSweep {
			continue
		}
		sweeps++
		w, ok := want[rule.Name]
		if !ok || rule.Period != w.period || rule.Action != w.action || rule.Table != w.table || rule.RelatedTable != w.related || rule.Version != 1 {
			t.Fatalf("%s: %s %s %s %s v%d", rule.Name, rule.Period, rule.Action, rule.Table, rule.RelatedTable, rule.Version)
		}
	}
	if sweeps != len(want) || RuleSetVersion != 1 {
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
