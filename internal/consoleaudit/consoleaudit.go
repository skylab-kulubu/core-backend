// Package consoleaudit brings the changes made in Keycloak's own console
// into core's Team audit record and its privilege notice
// (docs/console-audit.md; grup-yonetimi 10, dalga 2 A6).
//
// core reads Keycloak's admin event store (GET …/admin-events,
// view-events) every Interval, from where it stopped. Each event of a write
// made by anyone but core's own service account becomes one finished row of
// team_audit_records (via console), written once per event; a new row of a
// Privileged Group's membership or of a Group's role mappings goes to the
// privilege notice and the membership mail, as the same change made in the
// panel does. Nothing is undone: the console stays the emergency door
// (GY4), and this is its detection.
package consoleaudit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
)

// ResourceTypes are the admin events the reader asks Keycloak for.
var ResourceTypes = []string{
	"GROUP", "GROUP_MEMBERSHIP", "REALM_ROLE_MAPPING", "CLIENT_ROLE_MAPPING",
	"CLIENT", "CLIENT_ROLE", "REALM_ROLE",
}

const (
	// DefaultInterval is how often the store is read.
	DefaultInterval = time.Minute
	// MinInterval and MaxInterval bound KEYCLOAK_CONSOLE_AUDIT_INTERVAL.
	MinInterval = 15 * time.Second
	MaxInterval = 10 * time.Minute
	// PageSize is how many events one read asks for; MaxPages bounds the
	// reads of one pass (the next pass goes on).
	PageSize = 200
	MaxPages = 10
	// Retention is how long Keycloak keeps admin events (ADR-0062): a
	// cursor older than RetentionWarn may have missed some.
	Retention     = 30 * 24 * time.Hour
	RetentionWarn = 29 * 24 * time.Hour
	// maxCursorIDs bounds the ids kept for the cursor's millisecond.
	maxCursorIDs = 1000
	// passTimeout bounds one pass.
	passTimeout = 2 * time.Minute
)

// Source reads Keycloak's admin events (identity.Keycloak).
type Source interface {
	AdminEvents(ctx context.Context, fromMillis int64, max int, resourceTypes []string) ([]identity.AdminEvent, error)
	// ServiceAccountSubject is core's own subject: its events are skipped.
	ServiceAccountSubject(ctx context.Context) (uuid.UUID, error)
	// ClientIDOf names a client by its Keycloak UUID.
	ClientIDOf(ctx context.Context, clientUUID string) (string, error)
	// GetGroup reads a Group by id, for its path.
	GetGroup(ctx context.Context, idOrPath string) (identity.Group, error)
}

// Cursor is where the reader stands: the newest event time it has read
// (epoch milliseconds) and the ids it has read at that time.
type Cursor struct {
	Time int64
	IDs  []string
}

// advance is c past e.
func (c Cursor) advance(e identity.AdminEvent) Cursor {
	switch {
	case e.Time > c.Time:
		return Cursor{Time: e.Time, IDs: []string{e.ID}}
	case e.Time == c.Time && !slices.Contains(c.IDs, e.ID) && len(c.IDs) < maxCursorIDs:
		return Cursor{Time: c.Time, IDs: append(slices.Clone(c.IDs), e.ID)}
	}
	return c
}

// seen reports whether e is at or before c.
func (c Cursor) seen(e identity.AdminEvent) bool {
	return e.Time < c.Time || (e.Time == c.Time && slices.Contains(c.IDs, e.ID))
}

// ErrLocked is a pass another core holds.
var ErrLocked = errors.New("consoleaudit: another core reads")

// Store keeps the cursor (PostgresStore keeps it in console_audit_cursor).
type Store interface {
	// Locked runs fn while it holds the reader's lock, ErrLocked without.
	Locked(ctx context.Context, fn func(ctx context.Context) error) error
	// Load is the cursor, false before the first pass.
	Load(ctx context.Context) (Cursor, bool, error)
	Save(ctx context.Context, c Cursor) error
}

// Config is what the reader needs. Membership and Roles may be nil (the
// membership mail and the privilege notice off): the rows are written all
// the same.
type Config struct {
	Source     Source
	Store      Store
	Audit      teamaudit.ConsoleRecorder
	Membership identity.MembershipNotifier
	Roles      identity.GroupRolesNotifier
	Interval   time.Duration
	Now        func() time.Time
	Logf       func(string, ...any)
}

// Service reads the console's changes.
type Service struct {
	cfg  Config
	self uuid.UUID

	mu      sync.Mutex
	events  map[string]int
	passes  map[string]int
	gaps    int
	direct  int
	lastOK  time.Time
	lagSecs float64
}

// NewService checks cfg.
func NewService(cfg Config) (*Service, error) {
	if cfg.Source == nil || cfg.Store == nil || cfg.Audit == nil {
		return nil, errors.New("consoleaudit: source, store and audit are required")
	}
	if cfg.Interval == 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	return &Service{cfg: cfg, events: map[string]int{}, passes: map[string]int{}}, nil
}

// Outcomes of one event (skylab_console_audit_events_total).
const (
	outcomeRecorded      = "recorded"
	outcomeDuplicate     = "duplicate"
	outcomeSkippedSelf   = "skipped_self"
	outcomeSkippedError  = "skipped_error"
	outcomeUnsupported   = "unsupported"
	outcomeWriteFailed   = "write_failed"
	passOK, passFailed   = "ok", "failed"
	passLocked           = "locked"
	clientConsoleMaster  = "keycloak:master"
	clientConsoleRealm   = "keycloak:realm"
	unknownGroupPath     = "/"
	compositesPathSuffix = "/composites"
)

// Pass reads what Keycloak has from the cursor on, at most MaxPages pages.
// The first pass sets the cursor to now: what came before is not read.
func (s *Service) Pass(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, passTimeout)
	defer cancel()
	err := s.cfg.Store.Locked(ctx, s.pass)
	switch {
	case errors.Is(err, ErrLocked):
		s.countPass(passLocked)
		return nil
	case err != nil:
		s.countPass(passFailed)
		return err
	}
	s.countPass(passOK)
	return nil
}

func (s *Service) pass(ctx context.Context) error {
	now := s.cfg.Now()
	cursor, ok, err := s.cfg.Store.Load(ctx)
	if err != nil {
		return fmt.Errorf("consoleaudit: cursor: %w", err)
	}
	if !ok {
		s.cfg.Logf("keycloak console audit: first pass; changes from now on are read (%s)", now.UTC().Format(time.RFC3339))
		return s.cfg.Store.Save(ctx, Cursor{Time: now.UnixMilli()})
	}
	if s.self == uuid.Nil {
		self, err := s.cfg.Source.ServiceAccountSubject(ctx)
		if err != nil {
			return fmt.Errorf("consoleaudit: core's own subject: %w", err)
		}
		s.self = self
	}
	if age := now.Sub(time.UnixMilli(cursor.Time)); age > RetentionWarn {
		s.count(func() { s.gaps++ })
		s.cfg.Logf("keycloak console audit: WARN the last event read is %d days old; Keycloak keeps %d days, so changes may have been missed", int(age.Hours()/24), int(Retention.Hours()/24))
	}
	for page := 0; page < MaxPages; page++ {
		events, err := s.cfg.Source.AdminEvents(ctx, cursor.Time, PageSize, ResourceTypes)
		if err != nil {
			return fmt.Errorf("consoleaudit: read: %w", err)
		}
		fresh := 0
		for _, e := range events {
			if cursor.seen(e) {
				continue
			}
			fresh++
			if err := s.handle(ctx, e); err != nil {
				// The cursor stays before e: the next pass reads it again.
				if saveErr := s.cfg.Store.Save(ctx, cursor); saveErr != nil {
					return saveErr
				}
				return err
			}
			cursor = cursor.advance(e)
		}
		if fresh == 0 && len(events) == PageSize {
			// A full page all at one millisecond, every event read: step
			// past it rather than read the same page for ever.
			s.cfg.Logf("keycloak console audit: WARN more than %d events at one millisecond; stepping past it", PageSize)
			cursor = Cursor{Time: cursor.Time + 1}
		}
		if err := s.cfg.Store.Save(ctx, cursor); err != nil {
			return fmt.Errorf("consoleaudit: cursor: %w", err)
		}
		if len(events) < PageSize {
			break
		}
	}
	s.count(func() {
		s.lastOK = now
		s.lagSecs = max(0, now.Sub(time.UnixMilli(cursor.Time)).Seconds())
	})
	return nil
}

// handle writes e's row and, for a new one, tells the notifiers. An error
// is a row that could not be written: the pass stops before e.
func (s *Service) handle(ctx context.Context, e identity.AdminEvent) error {
	switch {
	case e.Error != "":
		s.countEvent(outcomeSkippedError)
		return nil
	case e.AuthUserID != "" && e.AuthUserID == s.self.String():
		// core's own write: its row names the person already.
		s.countEvent(outcomeSkippedSelf)
		return nil
	}
	actor, err := uuid.Parse(e.AuthUserID)
	if err != nil || actor == uuid.Nil {
		s.countEvent(outcomeUnsupported)
		s.cfg.Logf("keycloak console audit: an admin event without an author (%s %s) is not recorded", e.ResourceType, e.OperationType)
		return nil
	}
	c, ok := s.classify(ctx, e)
	if !ok {
		s.countEvent(outcomeUnsupported)
		return nil
	}
	c.record.Actor = teamaudit.Actor{ID: actor, Basis: teamaudit.BasisConsole, Client: clientConsoleRealm}
	if e.AuthRealmID != "" && e.AuthRealmID != e.RealmID {
		c.record.Actor.Client = clientConsoleMaster
	}
	c.record.Via = teamaudit.ViaConsole
	at := time.UnixMilli(e.Time).UTC()
	wrote, err := s.cfg.Audit.RecordConsole(ctx, c.record, at, e.ID)
	if err != nil {
		s.countEvent(outcomeWriteFailed)
		return fmt.Errorf("consoleaudit: record: %w", err)
	}
	if !wrote {
		s.countEvent(outcomeDuplicate)
		return nil
	}
	s.countEvent(outcomeRecorded)
	s.notify(ctx, c, actor.String(), at)
	return nil
}

// change is an event made a row, with what its notice needs.
type change struct {
	record  teamaudit.Record
	group   identity.Group
	added   []identity.ClientRole
	removed []identity.ClientRole
	// add is a membership change's direction.
	add bool
}

func (s *Service) notify(ctx context.Context, c change, actor string, at time.Time) {
	switch c.record.Action {
	case teamaudit.ActionMemberAdd, teamaudit.ActionMemberRemove:
		if s.cfg.Membership == nil || c.group.Path == unknownGroupPath {
			return
		}
		s.cfg.Membership.MembershipChanged(ctx, identity.MembershipChange{
			UserID: c.record.TargetUserID, Group: c.group, Added: c.add, ActorID: actor, Console: true, At: at,
		})
	case teamaudit.ActionGroupRoles:
		if s.cfg.Roles == nil || c.group.Path == unknownGroupPath {
			return
		}
		s.cfg.Roles.GroupRolesChanged(ctx, identity.GroupRolesChange{
			Group: c.group, Added: c.added, Removed: c.removed, ActorID: actor, Console: true, At: at,
		})
	case teamaudit.ActionUserRoles:
		// The groups-only model has no direct role: told in the log until
		// the notice has a template for it (docs/console-audit.md).
		s.count(func() { s.direct++ })
		s.cfg.Logf("keycloak console audit: WARN a role was %s a person directly in Keycloak's console (%d added, %d removed); see the audit record",
			map[bool]string{true: "mapped to", false: "unmapped from"}[len(c.added) > 0], len(c.added), len(c.removed))
	}
}

// classify makes e a row. False: an event the record does not keep.
func (s *Service) classify(ctx context.Context, e identity.AdminEvent) (change, bool) {
	parts := strings.Split(strings.Trim(e.ResourcePath, "/"), "/")
	op := e.OperationType
	switch e.ResourceType {
	case "GROUP_MEMBERSHIP":
		// users/{user}/groups/{group}
		if len(parts) != 4 || parts[0] != "users" || parts[2] != "groups" || (op != "CREATE" && op != "DELETE") {
			return change{}, false
		}
		user, err := uuid.Parse(parts[1])
		if err != nil {
			return change{}, false
		}
		group := s.group(ctx, parts[3], e.Representation)
		action := teamaudit.ActionMemberAdd
		if op == "DELETE" {
			action = teamaudit.ActionMemberRemove
		}
		return change{
			record: teamaudit.Record{Action: action, GroupID: group.ID, GroupPath: group.Path, TargetUserID: user},
			group:  group, add: op == "CREATE",
		}, true
	case "REALM_ROLE_MAPPING", "CLIENT_ROLE_MAPPING":
		// users|groups/{id}/role-mappings/realm, …/role-mappings/clients/{client}
		if len(parts) < 4 || parts[2] != "role-mappings" || (op != "CREATE" && op != "DELETE") {
			return change{}, false
		}
		clientUUID := ""
		if len(parts) >= 5 && parts[3] == "clients" {
			clientUUID = parts[4]
		}
		roles := s.roles(ctx, e.Representation, clientUUID)
		if len(roles) == 0 {
			return change{}, false
		}
		names := roleNames(roles)
		state := &teamaudit.State{Roles: names, Operation: op, ResourceType: e.ResourceType}
		c := change{}
		if op == "CREATE" {
			c.added = roles
			c.record.After = state
		} else {
			c.removed = roles
			c.record.Before = state
		}
		switch parts[0] {
		case "groups":
			group := s.group(ctx, parts[1], "")
			state.ID, state.Path = group.ID, group.Path
			c.group = group
			c.record.Action, c.record.GroupID, c.record.GroupPath = teamaudit.ActionGroupRoles, group.ID, group.Path
			return c, group.ID != ""
		case "users":
			user, err := uuid.Parse(parts[1])
			if err != nil {
				return change{}, false
			}
			c.record.Action, c.record.TargetUserID = teamaudit.ActionUserRoles, user
			return c, true
		}
		return change{}, false
	case "GROUP":
		// groups/{id}; groups/{parent}/children with the child in the
		// representation (a Sub-group made or moved); groups (moved to
		// the top).
		id := representedID(e.Representation)
		if id == "" {
			id = lastUUID(parts)
		}
		if id == "" {
			return change{}, false
		}
		group := s.group(ctx, id, e.Representation)
		state := &teamaudit.State{ID: group.ID, Path: group.Path, Code: group.Name, Operation: op, ResourceType: e.ResourceType}
		r := teamaudit.Record{GroupID: group.ID, GroupPath: group.Path}
		switch op {
		case "CREATE":
			r.Action, r.After = teamaudit.ActionTeamCreate, state
		case "UPDATE":
			r.Action, r.After = teamaudit.ActionGroupAttributes, state
		case "DELETE":
			r.Action, r.Before = teamaudit.ActionTeamDelete, state
		default:
			return change{}, false
		}
		return change{record: r, group: group}, true
	case "CLIENT":
		// clients/{client}
		if len(parts) < 2 || parts[0] != "clients" {
			return change{}, false
		}
		state := &teamaudit.State{ID: parts[1], Operation: op, ResourceType: e.ResourceType}
		state.Client = representedClientID(e.Representation)
		if state.Client == "" {
			state.Client = s.clientID(ctx, parts[1])
		}
		if len(parts) > 2 {
			// A sub-resource of the client (its scopes, its secret …):
			// its kind is kept, never its content.
			state.Kind = cleanSegment(parts[2])
		}
		return change{record: teamaudit.Record{Action: teamaudit.ActionKeycloakClient, After: state}}, true
	case "CLIENT_ROLE", "REALM_ROLE":
		return s.roleChange(ctx, e, parts)
	}
	return change{}, false
}

// roleChange is a client or realm role's own change (created, updated,
// deleted, composites added or removed): clients/{c}/roles/{name}[/composites],
// roles/{name}[/composites], roles-by-id/{id}[/composites].
func (s *Service) roleChange(ctx context.Context, e identity.AdminEvent, parts []string) (change, bool) {
	state := &teamaudit.State{Operation: e.OperationType, ResourceType: e.ResourceType}
	composite := strings.HasSuffix(e.ResourcePath, compositesPathSuffix)
	switch {
	case len(parts) >= 4 && parts[0] == "clients" && parts[2] == "roles":
		state.Client = s.clientID(ctx, parts[1])
		state.Role = parts[3]
	case len(parts) >= 2 && parts[0] == "roles":
		state.Role = parts[1]
	case len(parts) >= 2 && parts[0] == "roles-by-id":
		state.ID = parts[1]
	default:
		return change{}, false
	}
	if composite {
		state.Composites = roleNames(s.roles(ctx, e.Representation, ""))
	} else if state.Role == "" {
		var rep struct {
			Name string `json:"name"`
		}
		if json.Unmarshal([]byte(e.Representation), &rep) == nil {
			state.Role = rep.Name
		}
	}
	return change{record: teamaudit.Record{Action: teamaudit.ActionKeycloakRole, After: state}}, true
}

// group is the Group with id: its path from the event's representation,
// else from Keycloak; a Group gone (a deletion) keeps the path unknown.
func (s *Service) group(ctx context.Context, id, representation string) identity.Group {
	var rep struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Path string `json:"path"`
	}
	if representation != "" && json.Unmarshal([]byte(representation), &rep) == nil &&
		strings.HasPrefix(rep.Path, "/") && (rep.ID == "" || rep.ID == id) {
		return identity.Group{ID: id, Name: rep.Name, Path: rep.Path}
	}
	if g, err := s.cfg.Source.GetGroup(ctx, id); err == nil && strings.HasPrefix(g.Path, "/") {
		return g
	}
	return identity.Group{ID: id, Name: rep.Name, Path: unknownGroupPath}
}

// roles are the roles of a role mapping's (or a composite's)
// representation: a client role by its client's id, a realm role with
// none. clientUUID names the client of a mapping's path.
func (s *Service) roles(ctx context.Context, representation, clientUUID string) []identity.ClientRole {
	var reps []struct {
		Name        string `json:"name"`
		ClientRole  bool   `json:"clientRole"`
		ContainerID string `json:"containerId"`
	}
	if json.Unmarshal([]byte(representation), &reps) != nil {
		return nil
	}
	out := make([]identity.ClientRole, 0, len(reps))
	for _, r := range reps {
		if r.Name == "" {
			continue
		}
		// A realm role has no client: ":ROLE", as the notice keeps it.
		role := identity.ClientRole{Role: r.Name}
		if r.ClientRole {
			container := r.ContainerID
			if container == "" {
				container = clientUUID
			}
			role.ClientID = s.clientID(ctx, container)
			if role.ClientID == "" {
				role.ClientID = "?"
			}
		}
		out = append(out, role)
	}
	return out
}

// clientID names the client with Keycloak UUID id; "" when Keycloak cannot.
func (s *Service) clientID(ctx context.Context, id string) string {
	if id == "" {
		return ""
	}
	name, err := s.cfg.Source.ClientIDOf(ctx, id)
	if err != nil {
		return ""
	}
	return name
}

// roleNames are roles as "client:role", a realm role ":ROLE", sorted.
func roleNames(roles []identity.ClientRole) []string {
	out := make([]string, 0, len(roles))
	for _, r := range roles {
		out = append(out, r.ClientID+":"+r.Role)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// representedID is the id of a representation, "" when it has none.
func representedID(representation string) string {
	var rep struct {
		ID string `json:"id"`
	}
	if json.Unmarshal([]byte(representation), &rep) != nil {
		return ""
	}
	if _, err := uuid.Parse(rep.ID); err != nil {
		return ""
	}
	return rep.ID
}

// representedClientID is the clientId of a client's representation.
func representedClientID(representation string) string {
	var rep struct {
		ClientID string `json:"clientId"`
	}
	if json.Unmarshal([]byte(representation), &rep) != nil {
		return ""
	}
	return rep.ClientID
}

// lastUUID is the last path segment that is a UUID.
func lastUUID(parts []string) string {
	for i := len(parts) - 1; i >= 0; i-- {
		if _, err := uuid.Parse(parts[i]); err == nil {
			return parts[i]
		}
	}
	return ""
}

// cleanSegment is a path segment kept as a name: letters, digits, '-'
// and '_' only.
func cleanSegment(seg string) string {
	for _, r := range seg {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return ""
		}
	}
	return seg
}

// Run reads every Interval until ctx ends; the channel closes once it has
// stopped.
func (s *Service) Run(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(s.cfg.Interval)
		defer ticker.Stop()
		for {
			if err := s.Pass(ctx); err != nil && ctx.Err() == nil {
				s.cfg.Logf("keycloak console audit: pass failed, the next one goes on from the same place: %v", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return done
}

func (s *Service) count(update func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	update()
}

func (s *Service) countEvent(outcome string) { s.count(func() { s.events[outcome]++ }) }
func (s *Service) countPass(outcome string)  { s.count(func() { s.passes[outcome]++ }) }

// eventOutcomes and passOutcomes are every label the metrics carry.
var (
	eventOutcomes = []string{outcomeRecorded, outcomeDuplicate, outcomeSkippedSelf, outcomeSkippedError, outcomeUnsupported, outcomeWriteFailed}
	passOutcomes  = []string{passOK, passFailed, passLocked}
)

// Prometheus is the reader's metrics; no person, no path.
func (s *Service) Prometheus() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var b strings.Builder
	b.WriteString("# HELP skylab_console_audit_enabled Whether core reads Keycloak's console changes (KEYCLOAK_CONSOLE_AUDIT).\n# TYPE skylab_console_audit_enabled gauge\nskylab_console_audit_enabled 1\n")
	b.WriteString("# HELP skylab_console_audit_events_total Keycloak admin events read, by what became of them.\n# TYPE skylab_console_audit_events_total counter\n")
	for _, o := range eventOutcomes {
		fmt.Fprintf(&b, "skylab_console_audit_events_total{outcome=%q} %d\n", o, s.events[o])
	}
	b.WriteString("# HELP skylab_console_audit_pass_total Reads of Keycloak's admin events, by outcome.\n# TYPE skylab_console_audit_pass_total counter\n")
	for _, o := range passOutcomes {
		fmt.Fprintf(&b, "skylab_console_audit_pass_total{outcome=%q} %d\n", o, s.passes[o])
	}
	fmt.Fprintf(&b, "# HELP skylab_console_audit_gap_total Passes that found the cursor older than Keycloak keeps events.\n# TYPE skylab_console_audit_gap_total counter\nskylab_console_audit_gap_total %d\n", s.gaps)
	fmt.Fprintf(&b, "# HELP skylab_console_audit_direct_user_roles_total Roles mapped to or unmapped from a person directly in the console.\n# TYPE skylab_console_audit_direct_user_roles_total counter\nskylab_console_audit_direct_user_roles_total %d\n", s.direct)
	last := 0.0
	if !s.lastOK.IsZero() {
		last = float64(s.lastOK.Unix())
	}
	fmt.Fprintf(&b, "# HELP skylab_console_audit_last_success_timestamp_seconds When a pass last read to the end.\n# TYPE skylab_console_audit_last_success_timestamp_seconds gauge\nskylab_console_audit_last_success_timestamp_seconds %g\n", last)
	fmt.Fprintf(&b, "# HELP skylab_console_audit_lag_seconds How old the newest event read was at the last good pass.\n# TYPE skylab_console_audit_lag_seconds gauge\nskylab_console_audit_lag_seconds %g\n", s.lagSecs)
	return b.String()
}

// Off is the reader's metrics while it is off.
type Off struct{}

func (Off) Prometheus() string {
	return "# HELP skylab_console_audit_enabled Whether core reads Keycloak's console changes (KEYCLOAK_CONSOLE_AUDIT).\n# TYPE skylab_console_audit_enabled gauge\nskylab_console_audit_enabled 0\n"
}
