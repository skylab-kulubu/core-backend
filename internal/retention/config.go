// Package retention is core's periodic destruction run (ADR-0062,
// docs/retention-sweep.md): a daily sweep that empties the personal fields,
// or deletes the rows, its rules find past their retention period, and
// counts what core's hourly cleanups should already have removed.
//
// Every rule is one definition of the rows it is due for: the dry run
// counts them, apply changes them a batch at a time, and the record of each
// run (retention_runs, retention_run_rules, retention_periods) keeps only
// rule names, times and counts. One run at a time holds a database advisory
// lock; the schedule reads the last successful run from the database, so a
// restart or a release never resets it. A rule that would change more than a
// fifth of its table, or more than 50,000 rows, is refused and alarms.
//
// Nothing here writes a value a row held to a log line, an error, a metric
// or a record.
package retention

import (
	"fmt"
	"strings"
	"time"

	"github.com/skylab-kulubu/core-backend/internal/erasure"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/shorturl"
)

// Mode is what the scheduled sweep does.
type Mode string

const (
	// ModeOff runs nothing: no query, no metric. The default.
	ModeOff Mode = "off"
	// ModeDryRun counts what apply would change and changes nothing.
	ModeDryRun Mode = "dry-run"
	// ModeApply changes the rows.
	ModeApply Mode = "apply"
)

// ModeEnv holds the mode: off (or unset), dry-run or apply.
const ModeEnv = "RETENTION_SWEEP_MODE"

// ParseMode reads a mode. Anything but the three names is refused, so a typo
// never turns destruction on or off silently.
func ParseMode(raw string) (Mode, error) {
	switch strings.TrimSpace(raw) {
	case "", string(ModeOff):
		return ModeOff, nil
	case string(ModeDryRun):
		return ModeDryRun, nil
	case string(ModeApply):
		return ModeApply, nil
	default:
		return "", fmt.Errorf("%s must be off, dry-run or apply", ModeEnv)
	}
}

// Config is what the rules and the periods depend on.
type Config struct {
	// Mode is the configured mode. It also decides the windows of the hourly
	// cleanups the audits check (HourlyHitDeletion, ReadLinkWindow).
	Mode Mode
	// Period is PERIODIC_DESTRUCTION_INTERVAL: the length of a period.
	Period time.Duration
	// MediaRecoveryWindow is how long an archived Media keeps its object
	// (MEDIA_BLOB_RECOVERY_DAYS), for the media audit.
	MediaRecoveryWindow time.Duration
}

// ConfigFromEnv reads the mode and, unless it is off, the period. The media
// recovery window is the one core already parsed. Errors name variables,
// never values; with the mode off nothing else is read, so a release with
// the sweep off cannot fail to start because of it.
func ConfigFromEnv(getenv func(string) string, mediaRecoveryWindow time.Duration) (Config, error) {
	mode, err := ParseMode(getenv(ModeEnv))
	if err != nil {
		return Config{}, err
	}
	config := Config{Mode: mode, Period: erasure.DefaultPeriodicDestructionInterval, MediaRecoveryWindow: mediaRecoveryWindow}
	if config.MediaRecoveryWindow <= 0 {
		config.MediaRecoveryWindow = media.DefaultBlobRecoveryWindow
	}
	if mode == ModeOff {
		return config, nil
	}
	if config.Period, err = erasure.PeriodicDestructionIntervalFromEnv(getenv); err != nil {
		return Config{}, err
	}
	return config, nil
}

// HourlyHitDeletion reports whether the hourly short-link cleanup deletes
// click rows after shorturl.HitRetention (90 days). In apply mode it does
// not: click rows are kept and url_hits_scrub empties their personal fields
// after a year (ADR-0062). In the other modes nothing scrubs them, so the
// 90-day deletion stays, and switching apply off brings it back.
func HourlyHitDeletion(mode Mode) bool {
	return mode != ModeApply
}

// ReadLinkRecordRetention is how long the access log of private Media keeps
// its rows in apply mode (ADR-0062): three years, with the open's address
// emptied after one (read_link_ip).
const ReadLinkRecordRetention = 3 * 365 * 24 * time.Hour

// ReadLinkWindow is the age at which the hourly cleanup deletes the access
// log of private Media: three years in apply mode, where read_link_ip
// empties the address after one; one year (media.ReadLinkRetention, the
// address with the row) otherwise.
func ReadLinkWindow(mode Mode) time.Duration {
	if mode == ModeApply {
		return ReadLinkRecordRetention
	}
	return media.ReadLinkRetention
}

// hitDeletionWindow is the hourly short-link cleanup's window while it runs.
const hitDeletionWindow = shorturl.HitRetention
