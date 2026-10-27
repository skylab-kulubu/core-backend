// Package teamaudit is the audit record of Team and Group administration
// (docs/team-audit.md, ADR 0065): who changed which Group, when, and how.
// Every write first opens a pending row, then goes to Keycloak, then writes
// the outcome to the row. A row that cannot be opened stops the write:
// without a record there is no change.
//
// A row holds ids, Group paths and codes, never a name, an e-mail address or
// other personal field; the only free texts are a Team's own fields and the
// short note of an app assignment change, which the panel asks to keep free
// of personal data. Account erasure leaves the rows as they are; the
// retention sweep deletes them three years on (retention rule
// team_audit_age).
package teamaudit

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// Retention is how long a row is kept (S6): three years, after which the
// retention sweep deletes it.
const Retention = 3 * 365 * 24 * time.Hour

// Action is what was done.
type Action string

const (
	ActionMemberAdd       Action = "member.add"
	ActionMemberRemove    Action = "member.remove"
	ActionLeaderAdd       Action = "leader.add"
	ActionLeaderRemove    Action = "leader.remove"
	ActionTeamCreate      Action = "team.create"
	ActionTeamCode        Action = "team.code"
	ActionTeamProfile     Action = "team.profile"
	ActionTeamMove        Action = "team.move"
	ActionTeamDelete      Action = "team.delete"
	ActionGroupRoles      Action = "group.roles"
	ActionGroupAttributes Action = "group.attributes"
	// ActionAppAssign and ActionAppUnassign are an app role given to, or
	// taken from, a Group or a person from the panel's app assignments
	// (docs/app-assignments.md; Entra's "Add app role assignment to
	// group" and "Remove app role assignment from user"). The row's after
	// names the client and the role (State.Client, State.Role) and the
	// optional note; the target is the Group (GroupID, GroupPath) or the
	// person (TargetUserID, GroupPath empty), never both.
	ActionAppAssign   Action = "app.assign"
	ActionAppUnassign Action = "app.unassign"
	// ActionTermExtend, ActionTermHandover and ActionTermExpired are a
	// management membership term's (docs/management-membership-terms.md,
	// grup-yonetimi 11): its end moved, its holder handed it over, and the
	// daily job marking it expired (BasisSystem). The target is the
	// holder; before and after carry the end (State.EndsAt) and, for a
	// handover, the successor and how (State.SuccessorID, State.Handover).
	ActionTermExtend   Action = "membership.term.extend"
	ActionTermHandover Action = "membership.term.handover"
	ActionTermExpired  Action = "membership.term.expired"
)

// Actions are every action the table's check accepts.
var Actions = []Action{
	ActionMemberAdd, ActionMemberRemove, ActionLeaderAdd, ActionLeaderRemove,
	ActionTeamCreate, ActionTeamCode, ActionTeamProfile, ActionTeamMove, ActionTeamDelete,
	ActionGroupRoles, ActionGroupAttributes, ActionAppAssign, ActionAppUnassign,
	ActionTermExtend, ActionTermHandover, ActionTermExpired,
}

// targetsPerson reports whether the action is about one person: they, and
// only they, carry a target user.
func (a Action) targetsPerson() bool {
	switch a {
	case ActionMemberAdd, ActionMemberRemove, ActionLeaderAdd, ActionLeaderRemove,
		ActionTermExtend, ActionTermHandover, ActionTermExpired:
		return true
	}
	return false
}

// IsApp reports whether the action is an app assignment's: its target is a
// Group or a person.
func (a Action) IsApp() bool {
	return a == ActionAppAssign || a == ActionAppUnassign
}

// Outcome is how the write ended.
type Outcome string

const (
	// OutcomePending: the row is open and Keycloak was (or is being) asked.
	// A row left pending means core stopped between the two steps: the
	// outcome is unknown.
	OutcomePending Outcome = "pending"
	OutcomeOK      Outcome = "ok"
	OutcomeFailed  Outcome = "failed"
	// OutcomeDenied: authorization refused the write; Keycloak was not
	// asked (Writer.Deny).
	OutcomeDenied Outcome = "denied"
)

// Basis is why the actor may make the change.
type Basis string

const (
	// BasisPrivileged: ADMIN, YK, DK or the groups:manage role.
	BasisPrivileged Basis = "privileged"
	// BasisLeader: the actor is a Leader of a Team the target sits in or
	// under (ticket 04); Actor.ViaGroupID names that Team.
	BasisLeader Basis = "leader"
	// BasisNone: the actor has no basis for the change. Only a denied row
	// (Writer.Deny) carries it: a write that is made always has one.
	BasisNone Basis = "none"
	// BasisSystem: core's own job made the change, no person
	// (membership.term.expired only). Its actor is the nil subject, and
	// Actor.Client names the job (SystemTermExpiry).
	BasisSystem Basis = "system"
)

// SystemTermExpiry is the Client of the rows the management term job
// writes (BasisSystem): the "aktör system:term-expiry" of grup-yonetimi 11.
const SystemTermExpiry = "system:term-expiry"

// Actor is who makes the change, as the request carries them.
type Actor struct {
	// ID is the caller's subject (the token's sub).
	ID    uuid.UUID
	Basis Basis
	// ViaGroupID is, for BasisLeader, the Team whose Leader group grants
	// the change.
	ViaGroupID string
	// Client is the Keycloak client the token was issued to (azp).
	Client string
	// CorrelationID is the request's id (X-Request-ID).
	CorrelationID string
}

type actorKey struct{}

// WithActor returns ctx carrying the actor of the Group writes made with it.
func WithActor(ctx context.Context, a Actor) context.Context {
	return context.WithValue(ctx, actorKey{}, a)
}

// ActorFrom is the actor ctx carries.
func ActorFrom(ctx context.Context) (Actor, bool) {
	a, ok := ctx.Value(actorKey{}).(Actor)
	return a, ok
}

// RecordedAttributes are the Group attributes whose values a row keeps: a
// Team's names, descriptions and logo, its public and door flags and its
// settings (members_manage_competitors). Of
// any other attribute only the name is kept (State.OtherAttributes).
var RecordedAttributes = []string{
	"display_name_tr", "display_name_en", "description_tr", "description_en", "logo_media_id",
	"public_listing", "public_leaders", "team_door_scan", "members_manage_competitors",
}

// State is a Group's side of a change: what it was (Record.Before) or is to
// be (Record.After). Only ids, paths, codes, the recorded attributes and
// role names: never a person's name or address.
type State struct {
	ID       string `json:"id,omitempty"`
	Path     string `json:"path,omitempty"`
	ParentID string `json:"parentId,omitempty"`
	// Code is the Group's Keycloak name: the Team code.
	Code string `json:"code,omitempty"`
	// Attributes are values of RecordedAttributes.
	Attributes map[string]string `json:"attributes,omitempty"`
	// OtherAttributes are the names of other attributes that changed.
	OtherAttributes []string `json:"otherAttributes,omitempty"`
	// Roles are client roles as "client:role".
	Roles []string `json:"roles,omitempty"`
	// UnverifiedAck is set on a member.add to a Privileged Group of a
	// person who is not YTÜ-linked (grup-yonetimi 07): whether the caller
	// confirmed the panel's warning. Absent on every other row.
	UnverifiedAck *bool `json:"unverifiedAck,omitempty"`
	// Client and Role are the app role of an app.assign or app.unassign
	// row; absent on every other row.
	Client string `json:"client,omitempty"`
	Role   string `json:"role,omitempty"`
	// EndsAt is a management membership term's end (RFC 3339), on a
	// membership.term.* row; absent on every other row.
	EndsAt string `json:"endsAt,omitempty"`
	// SuccessorID and Handover are a membership.term.handover's: who the
	// term was handed to (a subject) and how ("immediate": the holder was
	// taken out at once; "at_term_end": marked at their own end).
	SuccessorID string `json:"successorId,omitempty"`
	Handover    string `json:"handover,omitempty"`
}

// Limits of what a row holds. A longer text is cut, so a long value never
// stops a write; a path longer than MaxPath is refused (Keycloak paths are
// far shorter, SPEC §4.5).
const (
	MaxPath          = 1024
	MaxText          = 280
	MaxIdentifier    = 255
	MaxAttributeName = 64
	MaxAttributes    = 32
	MaxRoles         = 256
	MaxCorrelationID = 128
)

// Record is one row as a write opens it.
type Record struct {
	Actor  Actor
	Action Action
	// GroupID is the target Group's Keycloak id. A team.create has none
	// until Keycloak answers; Finish sets it from the State it is given.
	GroupID string
	// GroupPath is the target Group's path at the time.
	GroupPath string
	// TargetUserID is the person of a member or leader action.
	TargetUserID uuid.UUID
	Before       *State
	After        *State
}

// ErrUnavailable is a row that could not be opened: the write must not go
// on (503 audit_unavailable).
var ErrUnavailable = errors.New("teamaudit: audit record unavailable")

// unavailable wraps cause as ErrUnavailable with only a code: a database
// error's message can quote the values of the statement.
func unavailable(cause string) error {
	return fmt.Errorf("%w (%s)", ErrUnavailable, cause)
}

var (
	errorCodePattern     = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)
	correlationIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
)

// ValidErrorCode reports whether code may stand in error_code.
func ValidErrorCode(code string) bool { return errorCodePattern.MatchString(code) }

// Writer opens and closes rows.
type Writer interface {
	// Begin opens a pending row for r and returns its id. An error is
	// ErrUnavailable: the write must not go on.
	Begin(ctx context.Context, r Record) (uuid.UUID, error)
	// Finish writes the outcome of the row id: ok, or failed with the
	// error's code. A non-nil after replaces the row's after, and its ID
	// becomes the row's Group id when the row has none (team.create).
	// Only a pending row is finished.
	Finish(ctx context.Context, id uuid.UUID, outcome Outcome, errorCode string, after *State) error
	// Deny writes one denied row for r: a write authorization refused,
	// with the refusal's code. Only write routes call it (ticket 04).
	Deny(ctx context.Context, r Record, errorCode string) error
}

// AppReader reads the app assignment rows of one client (the panel's
// "Denetim" tab and the assignments' "who, when"; docs/app-assignments.md).
type AppReader interface {
	// AppRows are the client's app.assign and app.unassign rows, every
	// outcome, newest first, at most limit.
	AppRows(ctx context.Context, client string, limit int) ([]Row, error)
	// LatestAppRows are, for each target and role of the client, the
	// newest app.assign or app.unassign row that ended ok.
	LatestAppRows(ctx context.Context, client string) ([]Row, error)
}

// AppKey is what an app row is about: its target (a Group's id or a
// person's) and its role. LatestAppRows answers one row per key.
func (r Row) AppKey() string {
	target := r.GroupID
	if r.TargetUserID != uuid.Nil {
		target = r.TargetUserID.String()
	}
	role := ""
	if r.After != nil {
		role = r.After.Role
	}
	return target + "\x00" + role
}

// checkOutcome refuses a row opened as outcome that its basis cannot have:
// BasisNone is for denied rows only.
func (r Record) checkOutcome(outcome Outcome) string {
	if r.Actor.Basis == BasisNone && outcome != OutcomeDenied {
		return "invalid_basis"
	}
	return ""
}

// normalize checks r and cuts its texts to their limits. The error is a
// short code.
func (r Record) normalize() (Record, string) {
	if r.Actor.ID == uuid.Nil && r.Actor.Basis != BasisSystem {
		return r, "no_actor"
	}
	switch r.Actor.Basis {
	case BasisPrivileged:
		if r.Actor.ViaGroupID != "" {
			return r, "invalid_basis"
		}
	case BasisLeader:
		if _, err := uuid.Parse(r.Actor.ViaGroupID); err != nil {
			return r, "invalid_basis"
		}
	case BasisNone:
		if r.Actor.ViaGroupID != "" {
			return r, "invalid_basis"
		}
	case BasisSystem:
		if r.Actor.ID != uuid.Nil || r.Actor.ViaGroupID != "" || r.Action != ActionTermExpired {
			return r, "invalid_basis"
		}
	default:
		return r, "invalid_basis"
	}
	if !slices.Contains(Actions, r.Action) {
		return r, "invalid_action"
	}
	if r.Action.IsApp() {
		// One target: a Group, by id and path, or a person, without a path.
		switch {
		case r.GroupID != "" && r.TargetUserID != uuid.Nil, r.GroupID == "" && r.TargetUserID == uuid.Nil:
			return r, "invalid_target"
		case r.TargetUserID != uuid.Nil && r.GroupPath != "":
			return r, "invalid_path"
		}
		if r.After == nil || r.After.Client == "" || r.After.Role == "" {
			return r, "no_app_role"
		}
	} else if r.GroupID == "" && r.Action != ActionTeamCreate {
		return r, "no_group"
	}
	if r.GroupID != "" {
		if _, err := uuid.Parse(r.GroupID); err != nil {
			return r, "invalid_group"
		}
	}
	if r.GroupID != "" || !r.Action.IsApp() {
		if !strings.HasPrefix(r.GroupPath, "/") || utf8.RuneCountInString(r.GroupPath) > MaxPath || !utf8.ValidString(r.GroupPath) {
			return r, "invalid_path"
		}
	}
	if !r.Action.IsApp() && r.Action.targetsPerson() != (r.TargetUserID != uuid.Nil) {
		return r, "invalid_target"
	}
	r.Actor.Client = cut(r.Actor.Client, MaxIdentifier)
	if !correlationIDPattern.MatchString(r.Actor.CorrelationID) {
		// The id may come from the caller's X-Request-ID: one that is not
		// a plain token is not kept.
		r.Actor.CorrelationID = ""
	}
	r.Before = r.Before.normalize()
	r.After = r.After.normalize()
	return r, ""
}

func (s *State) normalize() *State {
	if s == nil {
		return nil
	}
	out := State{
		ID:       cut(s.ID, MaxIdentifier),
		Path:     cut(s.Path, MaxPath),
		ParentID: cut(s.ParentID, MaxIdentifier),
		Code:     cut(s.Code, MaxIdentifier),
		Client:   cut(s.Client, MaxIdentifier),
		Role:     cut(s.Role, MaxIdentifier),
		EndsAt:   cut(s.EndsAt, MaxIdentifier),
		Handover: cut(s.Handover, MaxIdentifier),
	}
	if _, err := uuid.Parse(s.SuccessorID); err == nil {
		out.SuccessorID = s.SuccessorID
	}
	if s.UnverifiedAck != nil {
		ack := *s.UnverifiedAck
		out.UnverifiedAck = &ack
	}
	for _, name := range RecordedAttributes {
		if v, ok := s.Attributes[name]; ok {
			if out.Attributes == nil {
				out.Attributes = map[string]string{}
			}
			out.Attributes[name] = cut(v, MaxText)
		}
	}
	others := slices.Sorted(slices.Values(s.OtherAttributes))
	for _, name := range slices.Compact(others) {
		if len(out.OtherAttributes) == MaxAttributes {
			break
		}
		out.OtherAttributes = append(out.OtherAttributes, cut(name, MaxAttributeName))
	}
	roles := slices.Sorted(slices.Values(s.Roles))
	for _, role := range slices.Compact(roles) {
		if len(out.Roles) == MaxRoles {
			break
		}
		out.Roles = append(out.Roles, cut(role, MaxIdentifier))
	}
	return &out
}

// cut is s without invalid UTF-8, at most n runes long.
func cut(s string, n int) string {
	s = strings.ToValidUTF8(s, "")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// AttributeChange is the Before and After a change of attributes from old to
// next records: the recorded attributes that differ, with their values, and
// the names of the other attributes that differ. Both are nil when nothing
// differs.
func AttributeChange(old, next map[string]string) (before, after map[string]string, others []string) {
	names := map[string]struct{}{}
	for name := range old {
		names[name] = struct{}{}
	}
	for name := range next {
		names[name] = struct{}{}
	}
	for _, name := range slices.Sorted(maps.Keys(names)) {
		was, had := old[name]
		is, has := next[name]
		if had == has && was == is {
			continue
		}
		if !slices.Contains(RecordedAttributes, name) {
			others = append(others, name)
			continue
		}
		if had {
			if before == nil {
				before = map[string]string{}
			}
			before[name] = was
		}
		if has {
			if after == nil {
				after = map[string]string{}
			}
			after[name] = is
		}
	}
	return before, after, others
}
