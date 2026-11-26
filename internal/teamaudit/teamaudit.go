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
	// ActionTicketResend and ActionTicketLinkRenew are a Ticket's mail sent
	// again and its link renewed from the panel (docs/ticket-resend.md).
	// The target is the Event's team (GroupID, GroupPath); after names the
	// Ticket and the Event (State.TicketID, State.EventID), never a person.
	ActionTicketResend    Action = "ticket.resend"
	ActionTicketLinkRenew Action = "ticket.link.renew"
	// ActionTicketCancel and ActionTicketRestore are a Ticket cancelled by an
	// operator and the cancellation taken back (docs/ticket-cancellation.md),
	// recorded as ActionTicketResend: the Event's team, the Ticket and the
	// Event, never a person nor the reason.
	ActionTicketCancel  Action = "ticket.cancel"
	ActionTicketRestore Action = "ticket.restore"
	// ActionTicketPromote is an operator moving a waitlisted Ticket to
	// active (docs/event-capacity.md), recorded as ActionTicketCancel.
	ActionTicketPromote Action = "ticket.promote"
	// ActionTicketImport is an operator's import of a list of attendees
	// (docs/attendee-import.md), recorded as ActionTicketCancel but
	// without a Ticket: the Event's team, the Event and the import's
	// counts (State.Import), never a name or an e-mail address.
	ActionTicketImport Action = "ticket.import"
	// ActionTicketImportPreview is the import's preview, recorded as
	// ActionTicketImport: what the list would do, nothing written.
	ActionTicketImportPreview Action = "ticket.import.preview"
	// ActionCoOrganizerAdd, ActionCoOrganizerRemove and
	// ActionCoOrganizerChange are an Event's Co-organizer list changed
	// (docs/co-organizers.md): a team or a person added or removed, or a
	// team's choices changed. The target is the Event's Owner team
	// (GroupID, GroupPath), none for an Event without one; before and after
	// name the Event and the team (State.Code, State.Management,
	// State.Visibility) or the person (State.PersonID), never a name.
	ActionCoOrganizerAdd    Action = "co_organizer.add"
	ActionCoOrganizerRemove Action = "co_organizer.remove"
	ActionCoOrganizerChange Action = "co_organizer.change"
	// The user.* actions are a person's account changed from the panel's
	// person card (docs/user-accounts.md): the target is the person
	// (TargetUserID), with no Group (GroupID and GroupPath empty). before
	// and after carry flags and ids only (State's account fields), never
	// an address, a name or a password.
	ActionUserEnable           Action = "user.enable"
	ActionUserDisable          Action = "user.disable"
	ActionUserPasswordSet      Action = "user.password.set"
	ActionUserPasswordMail     Action = "user.password.mail"
	ActionUserEmailChange      Action = "user.email.change"
	ActionUserEmailVerify      Action = "user.email.verify"
	ActionUserEmailMail        Action = "user.email.mail"
	ActionUserEmailPrimary     Action = "user.email.primary"
	ActionUserRequiredActions  Action = "user.required_actions"
	ActionUserCredentialRemove Action = "user.credential.remove"
	ActionUserIdentityUnlink   Action = "user.idp.unlink"
	ActionUserLockoutClear     Action = "user.lockout.clear"
	ActionUserProfile          Action = "user.profile"
	// ActionUserRoles is a role mapped to, or unmapped from, a person
	// directly (not through a Group) in Keycloak's console
	// (docs/console-audit.md): the groups-only model has none, so each
	// one is worth a row.
	ActionUserRoles Action = "user.roles"
)

// The keycloak.* actions are Keycloak configuration changed in its own
// console (docs/console-audit.md): a client, or a client or realm role
// (its creation, deletion or composites). No Group, no target person.
const (
	ActionKeycloakClient Action = "keycloak.client"
	ActionKeycloakRole   Action = "keycloak.role"
)

// UserActions are the user.* actions, in the order the table's check lists
// them.
var UserActions = []Action{
	ActionUserEnable, ActionUserDisable, ActionUserPasswordSet, ActionUserPasswordMail,
	ActionUserEmailChange, ActionUserEmailVerify, ActionUserEmailMail, ActionUserEmailPrimary,
	ActionUserRequiredActions, ActionUserCredentialRemove, ActionUserIdentityUnlink,
	ActionUserLockoutClear, ActionUserProfile, ActionUserRoles,
}

// Actions are every action the table's check accepts.
var Actions = []Action{
	ActionMemberAdd, ActionMemberRemove, ActionLeaderAdd, ActionLeaderRemove,
	ActionTeamCreate, ActionTeamCode, ActionTeamProfile, ActionTeamMove, ActionTeamDelete,
	ActionGroupRoles, ActionGroupAttributes, ActionAppAssign, ActionAppUnassign,
	ActionTermExtend, ActionTermHandover, ActionTermExpired,
	ActionTicketResend, ActionTicketLinkRenew, ActionTicketCancel, ActionTicketRestore, ActionTicketPromote,
	ActionTicketImport, ActionTicketImportPreview,
	ActionCoOrganizerAdd, ActionCoOrganizerRemove, ActionCoOrganizerChange,
	ActionUserEnable, ActionUserDisable, ActionUserPasswordSet, ActionUserPasswordMail,
	ActionUserEmailChange, ActionUserEmailVerify, ActionUserEmailMail, ActionUserEmailPrimary,
	ActionUserRequiredActions, ActionUserCredentialRemove, ActionUserIdentityUnlink,
	ActionUserLockoutClear, ActionUserProfile, ActionUserRoles,
	ActionKeycloakClient, ActionKeycloakRole,
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

// IsUser reports whether the action is a person's account change: its
// target is the person alone, with no Group.
func (a Action) IsUser() bool {
	return slices.Contains(UserActions, a)
}

// IsCoOrganizer reports whether the action is an Event's Co-organizer
// list's: its target is the Event's Owner team, or no Group for an Event
// without one.
func (a Action) IsCoOrganizer() bool {
	return a == ActionCoOrganizerAdd || a == ActionCoOrganizerRemove || a == ActionCoOrganizerChange
}

// IsKeycloak reports whether the action is a Keycloak configuration
// change's (keycloak.*): no Group and no target person.
func (a Action) IsKeycloak() bool {
	return a == ActionKeycloakClient || a == ActionKeycloakRole
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
	// BasisSystem: core's own job made the change, no person: the
	// management term job marking a term expired (membership.term.expired,
	// SystemTermExpiry) and taking its holder out of the Group 30 days
	// after its end (member.remove, SystemTermRemoval). Its actor is the
	// nil subject, and Actor.Client names the job.
	BasisSystem Basis = "system"
	// BasisEventEditor: the actor may edit the Event by the Event policy
	// (a member of a team whose members edit its Events, GECEKODU; one of
	// its Co-organizers, docs/co-organizers.md) without being Privileged or
	// a Leader of the Event's team or a Team above it. A copy's
	// co_organizer.add rows (reason "copy") by a Co-organizer carry it. A Ticket's
	// cancellation and its taking back (docs/ticket-cancellation.md), its
	// promotion and import, and its resend and link renewal by a
	// Co-organizer carry it; Actor.ViaGroupID names the Event's team.
	BasisEventEditor Basis = "event_editor"
	// BasisConsole is a change made in Keycloak's own console (or its Admin
	// REST API by anyone but core), read back from Keycloak's admin events
	// (docs/console-audit.md): the actor is whoever Keycloak names, in the
	// master realm or the club's; the row's via is console.
	BasisConsole Basis = "console"
)

// SystemTermExpiry is the Client of the rows the management term job
// writes (BasisSystem): the "aktör system:term-expiry" of grup-yonetimi 11.
const SystemTermExpiry = "system:term-expiry"

// SystemTermRemoval is the Client of the member.remove rows the management
// term job writes when it takes an expired term's holder out of the Group,
// 30 days after the term's end (ADR-0078 karar 3, 2026-11-24).
const SystemTermRemoval = "system:term-removal"

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

// Via is the way a change came (the via column, migration
// `20261123200000`): a member add an accepted Invitation made, one an
// approved Access request made, a change an Access review decided. Empty
// is a change made directly, as every row before the column. The actor
// and the basis stay those of the person who made or decided the change.
type Via string

const (
	ViaInvitation    Via = "invitation"
	ViaAccessRequest Via = "access_request"
	ViaAccessReview  Via = "access_review"
	// ViaConsole is a change made in Keycloak's console, outside core
	// (docs/console-audit.md); its basis is BasisConsole.
	ViaConsole Via = "console"
)

// Vias are every way the table's check accepts.
var Vias = []Via{ViaInvitation, ViaAccessRequest, ViaAccessReview, ViaConsole}

type viaKey struct{}

// WithVia returns ctx whose Group writes are recorded as having come by v:
// every row a writer opens with it (Begin, Deny) carries v unless the
// Record names its own. The writes themselves read it nowhere else.
func WithVia(ctx context.Context, v Via) context.Context {
	return context.WithValue(ctx, viaKey{}, v)
}

// ViaFrom is the way ctx carries, empty for none.
func ViaFrom(ctx context.Context) Via {
	v, _ := ctx.Value(viaKey{}).(Via)
	return v
}

// withVia is r with the way ctx carries when r names none.
func (r Record) withVia(ctx context.Context) Record {
	if r.Via == "" {
		r.Via = ViaFrom(ctx)
	}
	return r
}

// RecordedAttributes are the Group attributes whose values a row keeps: a
// Team's names, descriptions and logo, its public and door flags and its
// settings (team_competition_manage; members_manage_competitors, retired
// 2026-11-23, stays recorded should a Privileged write change it). Of
// any other attribute only the name is kept (State.OtherAttributes).
var RecordedAttributes = []string{
	"display_name_tr", "display_name_en", "description_tr", "description_en", "logo_media_id",
	"public_listing", "public_leaders", "team_door_scan", "members_manage_competitors",
	"team_competition_manage",
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
	// Kind is the Leader group of a leader.add or leader.remove row
	// (LIDERLER or KOORDINATORLER); absent on every other row.
	Kind string `json:"kind,omitempty"`
	// TicketID and EventID are a ticket.* row's Ticket and its Event;
	// Resend is the resend's number (the mail's key …:r<n>), 0 for a
	// renewal without its mail. Absent on every other row.
	TicketID string `json:"ticketId,omitempty"`
	EventID  string `json:"eventId,omitempty"`
	Resend   int    `json:"resend,omitempty"`
	// The account fields of a user.* row; absent on every other row. Enabled
	// is the account's sign-in switch; Temporary and Generated a password's
	// (whether the person must change it, whether core made it up);
	// Verified the Primary e-mail's flag; Primary which address is primary
	// ("school" or "personal"); RequiredActions Keycloak's required action
	// aliases; CredentialID and CredentialType a credential's id and type
	// (password, otp, webauthn-passwordless …); Provider an identity
	// provider's alias; Fields the names of the profile fields a
	// user.profile changed. Never an address, a name or a password.
	Enabled         *bool    `json:"enabled,omitempty"`
	Temporary       *bool    `json:"temporary,omitempty"`
	Generated       *bool    `json:"generated,omitempty"`
	Verified        *bool    `json:"verified,omitempty"`
	Primary         string   `json:"primary,omitempty"`
	RequiredActions []string `json:"requiredActions,omitempty"`
	CredentialID    string   `json:"credentialId,omitempty"`
	CredentialType  string   `json:"credentialType,omitempty"`
	Provider        string   `json:"provider,omitempty"`
	Fields          []string `json:"fields,omitempty"`
	// Management, Visibility and PersonID are a co_organizer.* row's: a
	// Co-organizer team's choices (its code is Code), or the Co-organizer
	// person (a subject). Absent on every other row.
	Management string `json:"management,omitempty"`
	Visibility string `json:"visibility,omitempty"`
	PersonID   string `json:"personId,omitempty"`
	// Changes is how many changes a refused write of the list asked for
	// (its one denied row); Reason is why a row was written without a
	// write of the list ("copy": an Event's copy carried it). Absent on
	// every other row.
	Changes int    `json:"changes,omitempty"`
	Reason  string `json:"reason,omitempty"`
	// Import is a ticket.import row's counts; absent on every other row.
	Import *ImportCounts `json:"import,omitempty"`
	// Operation and ResourceType are Keycloak's names of a console change
	// (CREATE, UPDATE, DELETE, ACTION; GROUP, CLIENT, CLIENT_ROLE …), on a
	// via console row; Composites the roles a role's composites gained or
	// lost, as "client:role" (a realm role ":ROLE"), on a keycloak.role
	// row. Absent on every other row.
	Operation    string   `json:"operation,omitempty"`
	ResourceType string   `json:"resourceType,omitempty"`
	Composites   []string `json:"composites,omitempty"`
}

// ImportCounts are what an attendee import did (docs/attendee-import.md):
// counts and the mail switch only, never a row's content.
type ImportCounts struct {
	// Rows are the list's rows, the header left out.
	Rows int `json:"rows"`
	// Created are the Tickets written active, Waitlisted those written on
	// the waiting list (on a preview: that would be); Members of them went
	// to an account holder, Guests to nobody's account.
	Created    int `json:"created"`
	Waitlisted int `json:"waitlisted"`
	Members    int `json:"members"`
	Guests     int `json:"guests"`
	// Existing already had a Ticket on the Event (cancelled ones
	// included); Full found the Event full with its waiting list off.
	Existing int `json:"existing"`
	Full     int `json:"full"`
	// Invalid and Duplicates were left out of the list before anything
	// was written.
	Invalid    int `json:"invalid"`
	Duplicates int `json:"duplicates"`
	// Mail is whether the new Tickets' mail was asked for.
	Mail bool `json:"mail"`
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
	// Via is the way the change came; empty is direct. A writer takes the
	// one ctx carries (WithVia) when it is empty.
	Via Via
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
		// Each job its one action: the expiry mark, and the removal.
		job := (r.Action == ActionTermExpired && r.Actor.Client == SystemTermExpiry) ||
			(r.Action == ActionMemberRemove && r.Actor.Client == SystemTermRemoval)
		if r.Actor.ID != uuid.Nil || r.Actor.ViaGroupID != "" || !job {
			return r, "invalid_basis"
		}
	case BasisEventEditor:
		if _, err := uuid.Parse(r.Actor.ViaGroupID); err != nil ||
			(r.Action != ActionTicketCancel && r.Action != ActionTicketRestore && r.Action != ActionTicketPromote &&
				r.Action != ActionTicketImport && r.Action != ActionTicketImportPreview &&
				r.Action != ActionTicketResend && r.Action != ActionTicketLinkRenew && r.Action != ActionCoOrganizerAdd) {
			return r, "invalid_basis"
		}
	case BasisConsole:
		if r.Actor.ViaGroupID != "" || r.Via != ViaConsole {
			return r, "invalid_basis"
		}
	default:
		return r, "invalid_basis"
	}
	if r.Via == ViaConsole && r.Actor.Basis != BasisConsole {
		return r, "invalid_basis"
	}
	if !slices.Contains(Actions, r.Action) {
		return r, "invalid_action"
	}
	if r.Via != "" && !slices.Contains(Vias, r.Via) {
		return r, "invalid_via"
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
	} else if r.Action.IsUser() {
		// The person alone: no Group, no path.
		if r.TargetUserID == uuid.Nil {
			return r, "invalid_target"
		}
		if r.GroupID != "" || r.GroupPath != "" {
			return r, "invalid_path"
		}
	} else if r.Action.IsKeycloak() {
		// Keycloak's configuration: no Group, no path, no person.
		if r.TargetUserID != uuid.Nil {
			return r, "invalid_target"
		}
		if r.GroupID != "" || r.GroupPath != "" {
			return r, "invalid_path"
		}
	} else if r.Action.IsCoOrganizer() && r.GroupID == "" {
		// An Event without an Owner team: no Group, no path.
		if r.GroupPath != "" {
			return r, "invalid_path"
		}
	} else if r.GroupID == "" && r.Action != ActionTeamCreate {
		return r, "no_group"
	}
	if r.GroupID != "" {
		if _, err := uuid.Parse(r.GroupID); err != nil {
			return r, "invalid_group"
		}
	}
	if r.GroupID != "" || (!r.Action.IsApp() && !r.Action.IsUser() && !r.Action.IsKeycloak() && !r.Action.IsCoOrganizer()) {
		if !strings.HasPrefix(r.GroupPath, "/") || utf8.RuneCountInString(r.GroupPath) > MaxPath || !utf8.ValidString(r.GroupPath) {
			return r, "invalid_path"
		}
	}
	if !r.Action.IsApp() && !r.Action.IsUser() && !r.Action.IsKeycloak() && r.Action.targetsPerson() != (r.TargetUserID != uuid.Nil) {
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
		Kind:     cut(s.Kind, MaxIdentifier),
		EndsAt:   cut(s.EndsAt, MaxIdentifier),
		Handover: cut(s.Handover, MaxIdentifier),
	}
	if _, err := uuid.Parse(s.SuccessorID); err == nil {
		out.SuccessorID = s.SuccessorID
	}
	if _, err := uuid.Parse(s.TicketID); err == nil {
		out.TicketID = s.TicketID
	}
	if _, err := uuid.Parse(s.EventID); err == nil {
		out.EventID = s.EventID
	}
	if _, err := uuid.Parse(s.PersonID); err == nil {
		out.PersonID = s.PersonID
	}
	out.Management = cut(s.Management, MaxIdentifier)
	out.Visibility = cut(s.Visibility, MaxIdentifier)
	if s.Changes > 0 {
		out.Changes = s.Changes
	}
	out.Reason = cut(s.Reason, MaxIdentifier)
	if s.Resend > 0 {
		out.Resend = s.Resend
	}
	if s.UnverifiedAck != nil {
		ack := *s.UnverifiedAck
		out.UnverifiedAck = &ack
	}
	out.Enabled = copyBool(s.Enabled)
	out.Temporary = copyBool(s.Temporary)
	out.Generated = copyBool(s.Generated)
	out.Verified = copyBool(s.Verified)
	out.Primary = cut(s.Primary, MaxIdentifier)
	out.CredentialID = cut(s.CredentialID, MaxIdentifier)
	out.CredentialType = cut(s.CredentialType, MaxIdentifier)
	out.Provider = cut(s.Provider, MaxIdentifier)
	out.RequiredActions = names(s.RequiredActions)
	out.Fields = names(s.Fields)
	out.Operation = cut(s.Operation, MaxIdentifier)
	out.ResourceType = cut(s.ResourceType, MaxIdentifier)
	composites := slices.Sorted(slices.Values(s.Composites))
	for _, role := range slices.Compact(composites) {
		if len(out.Composites) == MaxRoles {
			break
		}
		out.Composites = append(out.Composites, cut(role, MaxIdentifier))
	}
	if s.Import != nil {
		counts := *s.Import
		out.Import = &counts
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

func copyBool(b *bool) *bool {
	if b == nil {
		return nil
	}
	v := *b
	return &v
}

// names are short identifiers sorted, once each, cut to MaxIdentifier, at
// most MaxAttributes of them.
func names(in []string) []string {
	var out []string
	for _, name := range slices.Compact(slices.Sorted(slices.Values(in))) {
		if len(out) == MaxAttributes {
			break
		}
		out = append(out, cut(name, MaxIdentifier))
	}
	return out
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

// ConsoleRecorder writes the rows of changes made in Keycloak's console
// (docs/console-audit.md). Such a change has already happened: its row is
// written finished (ok) at once, with the time Keycloak gives, and each
// admin event once (keycloak_event_id): Record reports whether it wrote a
// new row, false for an event already recorded.
type ConsoleRecorder interface {
	RecordConsole(ctx context.Context, r Record, occurredAt time.Time, eventID string) (bool, error)
}

// MaxEventID bounds a Keycloak admin event id (a UUID in practice).
const MaxEventID = 64

// checkConsole refuses a console row that is not one: basis and via
// console, an event id, a time.
func (r Record) checkConsole(occurredAt time.Time, eventID string) string {
	if r.Actor.Basis != BasisConsole || r.Via != ViaConsole {
		return "invalid_basis"
	}
	if eventID == "" || len(eventID) > MaxEventID || !correlationIDPattern.MatchString(eventID) {
		return "invalid_event_id"
	}
	if occurredAt.IsZero() {
		return "invalid_time"
	}
	return ""
}
