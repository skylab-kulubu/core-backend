// Package membershipterm keeps the terms of management memberships
// (docs/management-membership-terms.md; grup-yonetimi 11, GY2): a
// membership of YK or DK (a Privileged root that is not ADMIN, or a Group
// under one) or of a Team's LIDERLER/KOORDINATORLER ends with the season.
//
// The end is only marked (Yusuf, 2026-10-12 22:15): core warns the person
// and those over them 14 days and 1 day before, writes expired_at when it
// passes, records it and tells ADMIN; it never takes anyone out of a Group
// by itself. Removing, extending and handing over are a person's, from the
// panel. ADMIN and ordinary Team memberships have no term.
package membershipterm

import (
	"errors"
	"fmt"
	"strings"
	"time"
	_ "time/tzdata" // A term ends at the end of an Istanbul day whatever the image carries.

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
)

// Mode is MANAGEMENT_TERMS_MODE.
type Mode string

const (
	// ModeOff (or unset, the default): nothing is read, written or sent;
	// the term routes are not served.
	ModeOff Mode = "off"
	// ModeDryRun: the daily job counts what apply would write and send,
	// and writes and sends nothing (the rollout).
	ModeDryRun Mode = "dry-run"
	// ModeApply: terms are kept, warned of, marked and listed; the routes
	// are served.
	ModeApply Mode = "apply"
)

// ModeEnv names the mode.
const ModeEnv = "MANAGEMENT_TERMS_MODE"

// WeeklySummaryEnv switches the weekly summary to ADMIN: on (or unset, the
// default) or off.
const WeeklySummaryEnv = "MANAGEMENT_TERMS_WEEKLY_SUMMARY"

// ModeFromEnv reads ModeEnv; any other value than off, dry-run or apply is
// an error that names the variable.
func ModeFromEnv(getenv func(string) string) (Mode, error) {
	switch raw := strings.TrimSpace(getenv(ModeEnv)); raw {
	case "", string(ModeOff):
		return ModeOff, nil
	case string(ModeDryRun):
		return ModeDryRun, nil
	case string(ModeApply):
		return ModeApply, nil
	default:
		return "", fmt.Errorf("%s must be off, dry-run or apply, not %q", ModeEnv, raw)
	}
}

// WeeklySummaryFromEnv reads WeeklySummaryEnv: on unless it says off.
func WeeklySummaryFromEnv(getenv func(string) string) (bool, error) {
	switch raw := strings.TrimSpace(getenv(WeeklySummaryEnv)); raw {
	case "", "on":
		return true, nil
	case "off":
		return false, nil
	default:
		return false, fmt.Errorf("%s must be on or off, not %q", WeeklySummaryEnv, raw)
	}
}

// Scope is why a membership has a term.
type Scope string

const (
	// ScopePrivileged: YK or DK, or a Group under one (a Privileged root
	// that is not ADMIN).
	ScopePrivileged Scope = "privileged"
	// ScopeLeader: a Team's LIDERLER or KOORDINATORLER (authz.TeamOf).
	ScopeLeader Scope = "leader"
)

// Rules say which Groups' memberships have a term.
type Rules struct {
	// PrivilegedRoots are AUTHZ_PRIVILEGED_ROOTS.
	PrivilegedRoots []string
	// LeaderSubgroups are the Leader groups' names (LIDERLER,
	// KOORDINATORLER).
	LeaderSubgroups []string
}

// DefaultRules are the default policy's roots and Leader groups.
func DefaultRules() Rules {
	p := authz.DefaultPolicy()
	return Rules{PrivilegedRoots: authz.DefaultPrivilegedRoots(), LeaderSubgroups: p.LeaderSubgroups}
}

// ScopeOf is the scope of a membership of the Group at path: a non-ADMIN
// Privileged root or a Group under one, or a Team's Leader group. ADMIN's
// trees, a Team's own Group, an area and anything else have none.
func (r Rules) ScopeOf(path string) (Scope, bool) {
	if root := authz.PrivilegedRoot(r.PrivilegedRoots, path); root != "" {
		if authz.RootLabel(root) == "ADMIN" {
			return "", false
		}
		return ScopePrivileged, true
	}
	if _, role, ok := authz.TeamOf(path, r.PrivilegedRoots, r.LeaderSubgroups); ok && role != "" {
		return ScopeLeader, true
	}
	return "", false
}

// Role is how a list and a mail name a term's Group: board (YK), audit
// (DK), privileged (another Privileged root) or team-leaders.
func (r Rules) Role(path string) string {
	if root := authz.PrivilegedRoot(r.PrivilegedRoots, path); root != "" {
		switch authz.RootLabel(root) {
		case "YK":
			return RoleBoard
		case "DK":
			return RoleAudit
		}
		return RolePrivileged
	}
	return RoleTeamLeaders
}

// The roles a term's Group has (Rules.Role), the list's root filter.
const (
	RoleBoard       = "board"
	RoleAudit       = "audit"
	RolePrivileged  = "privileged"
	RoleTeamLeaders = "team-leaders"
)

// adminRoots are the Privileged roots named ADMIN.
func (r Rules) adminRoots() []string {
	return r.rootsNamed("ADMIN")
}

func (r Rules) rootsNamed(label string) []string {
	var out []string
	for _, root := range r.PrivilegedRoots {
		if authz.RootLabel(root) == label {
			out = append(out, root)
		}
	}
	return out
}

// overseers are the Group paths whose trees are told of a term of the
// Group at path, beside the holder (grup-yonetimi 11): for YK and DK,
// ADMIN and the root itself (05's rule 2); for a Leader group, the Leader
// groups of the Team above (when there is one) and YK (Yusuf, 2026-10-19).
func (r Rules) overseers(path string) []string {
	if root := authz.PrivilegedRoot(r.PrivilegedRoots, path); root != "" {
		return append(r.adminRoots(), root)
	}
	var out []string
	team, _, ok := authz.TeamOf(path, r.PrivilegedRoots, r.LeaderSubgroups)
	if ok {
		parent := team[:strings.LastIndex(team, "/")]
		if above, role, ok := authz.TeamOf(parent, r.PrivilegedRoots, r.LeaderSubgroups); ok && role == "" && above == parent {
			for _, leader := range r.LeaderSubgroups {
				out = append(out, parent+"/"+leader)
			}
		}
	}
	return append(out, r.rootsNamed("YK")...)
}

// Status is a term as the list shows it.
type Status string

const (
	StatusActive Status = "active"
	// StatusExpiring: the term ends within WarnEarly.
	StatusExpiring Status = "expiring"
	// StatusExpired: the term's end has passed (marked or about to be).
	StatusExpired Status = "expired"
)

// WarnEarly and WarnLate are how long before its end a term is warned of.
const (
	WarnEarly = 14 * 24 * time.Hour
	WarnLate  = 24 * time.Hour
)

// MaxExtension bounds an extension when no later season is defined (Yusuf,
// 2026-10-19: one year).
const MaxExtension = 365 * 24 * time.Hour

// Term is a management membership's term. Ids only.
type Term struct {
	ID        uuid.UUID
	UserID    uuid.UUID
	GroupID   uuid.UUID
	GroupPath string
	Scope     Scope
	// EndsAt is nil when no active season gave it an end.
	EndsAt    *time.Time
	SeasonID  *uuid.UUID
	GrantedBy *uuid.UUID
	Source    Source
	StartedAt time.Time
	Warned14d *time.Time
	Warned1d  *time.Time
	ExpiredAt *time.Time
	EndedAt   *time.Time
	UpdatedAt time.Time
}

// Source is how core came to keep a term.
type Source string

const (
	// SourceRoute: a member add through core's routes.
	SourceRoute Source = "route"
	// SourceBackfill: found in Keycloak by the daily job (the first rollout,
	// or an add in Keycloak's console).
	SourceBackfill Source = "backfill"
	// SourceHandover: the successor of a handover.
	SourceHandover Source = "handover"
)

// Status is the term's status at now.
func (t Term) Status(now time.Time) Status {
	switch {
	case t.ExpiredAt != nil, t.EndsAt != nil && !now.Before(*t.EndsAt):
		return StatusExpired
	case t.EndsAt != nil && !now.Before(t.EndsAt.Add(-WarnEarly)):
		return StatusExpiring
	}
	return StatusActive
}

// Notice is which term mail a row is.
type Notice string

const (
	NoticeWarning14d    Notice = "warning_14d"
	NoticeWarning1d     Notice = "warning_1d"
	NoticeExpiredList   Notice = "expired_list"
	NoticeWeeklySummary Notice = "weekly_summary"
)

// GroupCount is one Group's line of the weekly summary.
type GroupCount struct {
	GroupID string `json:"groupId"`
	Path    string `json:"path"`
	Count   int    `json:"count"`
}

// Mail is one queued term mail to one recipient. Ids, paths and counts.
type Mail struct {
	ID          uuid.UUID
	RecipientID uuid.UUID
	Notice      Notice
	// TermID, SubjectID, GroupID, GroupPath and EndsAt are a warning's.
	TermID    *uuid.UUID
	SubjectID *uuid.UUID
	GroupID   *uuid.UUID
	GroupPath string
	EndsAt    *time.Time
	// TermIDs are an expired list's terms.
	TermIDs []uuid.UUID
	// Summary is a weekly summary's Groups.
	Summary       []GroupCount
	OccurredAt    time.Time
	NextAttemptAt time.Time
	Attempts      int
	ClaimedAt     time.Time
}

var (
	// ErrNotFound: no such term, or one that has ended.
	ErrNotFound = errors.New("membershipterm: not found")
	// ErrInvalid: an end that is not a date, or not in the future.
	ErrInvalid = errors.New("membershipterm: invalid")
	// ErrOff: the mode is not apply.
	ErrOff = errors.New("membershipterm: off")
)

// RefusalError is a request refused with a code (the problem's code).
type RefusalError struct {
	Status int
	Code   string
}

func (e *RefusalError) Error() string { return "membershipterm: " + e.Code }

// Refusal codes.
const (
	// CodeSelfExtension: nobody extends their own term, or hands it to
	// themselves.
	CodeSelfExtension = "self_extension"
	// CodeTermTooLong: the end is past the next season's end, or a year
	// on when there is no next season.
	CodeTermTooLong = "term_too_long"
	// CodeTermNotExpired: only an expired term is removed through its
	// route; any other is the member route's.
	CodeTermNotExpired = "term_not_expired"
)

const timeZone = "Europe/Istanbul"

// istanbul is the clock a term's day is read in.
var istanbul = mustLocation(timeZone)

func mustLocation(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return loc
}

// EndOfDay is the last second of t's day in Istanbul: a term ends at the
// end of its last day (grup-yonetimi 11).
func EndOfDay(t time.Time) time.Time {
	day := t.In(istanbul)
	return time.Date(day.Year(), day.Month(), day.Day(), 23, 59, 59, 0, istanbul).UTC()
}

// ParseDay reads an end given as a day, "2027-05-31", as the end of that
// Istanbul day.
func ParseDay(raw string) (time.Time, error) {
	day, err := time.ParseInLocation(time.DateOnly, strings.TrimSpace(raw), istanbul)
	if err != nil {
		return time.Time{}, ErrInvalid
	}
	return EndOfDay(day), nil
}
