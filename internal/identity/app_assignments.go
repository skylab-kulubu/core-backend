package identity

import (
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
	"github.com/skylab-kulubu/core-backend/internal/teamcode"
	"github.com/skylab-kulubu/core-backend/internal/user"
	"golang.org/x/sync/errgroup"
)

// App assignments (docs/app-assignments.md; ekip-yetkileri ticket 15;
// Microsoft Entra's Enterprise apps → an app → "Users and groups"). An app
// role of an app (authz.App, config/app-assignments.json) is assigned to a
// Team, a Sub-team or a Leader group of either, or to a person, and taken
// off again, by ADMIN and YK; DK reads. Every change is a row of the Team
// audit record (app.assign, app.unassign), and so is every refusal of a
// caller who may read the assignments. An admin role, a Privileged root,
// /ROLLER and a service account stay in code (Keycloak's reconciler): the
// list shows their mappings of the app's roles, locked.

// The kinds of an assignment's target.
const (
	AppTargetGroup = "group"
	AppTargetUser  = "user"
)

// The areas of an assignment: the panel changes the panel's, the
// reconciler the code's (YETKI-MATRISI §6.1).
const (
	AppAreaPanel = "panel"
	AppAreaCode  = "code"
)

// ErrAppNotAssigned is an app role taken off a target that does not hold
// it directly (404 not_assigned).
var ErrAppNotAssigned = fmt.Errorf("identity: app role not assigned: %w", ErrNotFound)

// appGroupTargetLimit bounds the Groups one target search answers.
const appGroupTargetLimit = 20

// appStatsAtOnce bounds the Group reads the assignment list makes at once.
const appStatsAtOnce = 4

// AppSummary is an app as GET /v1/apps answers it.
type AppSummary struct {
	ClientID           string   `json:"clientId"`
	Name               string   `json:"name"`
	AssignmentRequired bool     `json:"assignmentRequired"`
	Roles              []string `json:"roles"`
	SuggestedRoles     []string `json:"suggestedRoles"`
	// Groups and Users are how many Groups and people one of Roles is
	// mapped to directly, each once, the code's included.
	Groups int `json:"groups"`
	Users  int `json:"users"`
}

// AppActor is who made a change: the subject and core's name for them,
// empty when core has none to show.
type AppActor struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
}

// AppAssignment is one app role mapped directly to a Group or a person.
type AppAssignment struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	Role string `json:"role"`
	// Area is AppAreaPanel when the panel may take it off, AppAreaCode for
	// a mapping the code owns (a Privileged root, /ROLLER, a service
	// account, or a Group no app role belongs on).
	Area string `json:"area"`
	// Reason is why a code mapping is not the panel's (authz.Placement…).
	Reason string `json:"reason,omitempty"`
	// Path, Name and the counts are the Group's: its path, its Turkish
	// display name (the code without one), how many Groups are below it
	// (they inherit the role: Keycloak's subgroups do) and how many enabled
	// people it and they hold, each once. Name and Email are a person's.
	Path       string `json:"path,omitempty"`
	Name       string `json:"name"`
	Email      string `json:"email,omitempty"`
	InheritsTo int    `json:"inheritsTo"`
	Members    int    `json:"members"`
	// AssignedBy and AssignedAt are the last app.assign row that ended ok,
	// when no later app.unassign did; nil without one (made before the
	// panel, or in Keycloak's console) or for a caller who does not read
	// the audit record (AppAssignments.AuditVisible).
	AssignedBy *AppActor  `json:"assignedBy"`
	AssignedAt *time.Time `json:"assignedAt"`
}

// AppAssignments is GET /v1/apps/{clientId}/assignments.
type AppAssignments struct {
	App AppSummary `json:"app"`
	// AuditVisible is whether the caller reads the audit record
	// (audit:read): only then AssignedBy and AssignedAt say anything.
	AuditVisible bool            `json:"auditVisible"`
	Assignments  []AppAssignment `json:"assignments"`
}

// AppAssignmentFilter narrows the list: one Type, one Role.
type AppAssignmentFilter struct {
	Type string
	Role string
}

// AppAssignmentInput is one change: the target's kind and id, the role,
// and an optional note (at most teamaudit.MaxNote runes).
type AppAssignmentInput struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	Role string `json:"role"`
	Note string `json:"note"`
}

// AppAuditEntry is one app row of the Team audit record.
type AppAuditEntry struct {
	ID         uuid.UUID `json:"id"`
	OccurredAt time.Time `json:"occurredAt"`
	Action     string    `json:"action"`
	Outcome    string    `json:"outcome"`
	ErrorCode  string    `json:"errorCode,omitempty"`
	Actor      AppActor  `json:"actor"`
	Type       string    `json:"type"`
	TargetID   string    `json:"targetId"`
	Path       string    `json:"path,omitempty"`
	Name       string    `json:"name,omitempty"`
	Role       string    `json:"role"`
	Note       string    `json:"note,omitempty"`
}

// AppTarget is a Group or a person an app role may be assigned to, with
// the app's roles it already holds directly.
type AppTarget struct {
	Type          string   `json:"type"`
	ID            string   `json:"id"`
	Path          string   `json:"path,omitempty"`
	Name          string   `json:"name"`
	Email         string   `json:"email,omitempty"`
	InheritsTo    int      `json:"inheritsTo"`
	Members       int      `json:"members"`
	AssignedRoles []string `json:"assignedRoles"`
}

func (s *service) appOf(clientID string) (authz.App, error) {
	app, ok := authz.AppOf(s.apps, clientID)
	if !ok {
		return authz.App{}, ErrNotFound
	}
	return app, nil
}

func appSummary(app authz.App) AppSummary {
	return AppSummary{
		ClientID: app.Client, Name: app.Name, AssignmentRequired: app.AssignmentRequired,
		Roles: slices.Clone(app.Roles), SuggestedRoles: slices.Clone(app.SuggestedRoles),
	}
}

// appMapping is one role of an app and its direct mappings.
type appMapping struct {
	role   string
	groups []Group
	people []Person
}

func (s *service) appMappings(ctx context.Context, app authz.App, roles []string) ([]appMapping, error) {
	out := make([]appMapping, 0, len(roles))
	for _, role := range roles {
		groups, people, err := s.dir.ClientRoleMappings(ctx, app.Client, role)
		if err != nil {
			return nil, err
		}
		out = append(out, appMapping{role: role, groups: groups, people: people})
	}
	return out, nil
}

// Apps is GET /v1/apps: every app of the catalogue, with how many Groups
// and people its roles are mapped to.
func (s *service) Apps(ctx context.Context, p authz.Principal) ([]AppSummary, error) {
	if err := s.allow(p, authz.TypeAppAssignment, authz.Read); err != nil {
		return nil, err
	}
	out := make([]AppSummary, 0, len(s.apps))
	for _, app := range s.apps {
		mappings, err := s.appMappings(ctx, app, app.Roles)
		if err != nil {
			return nil, err
		}
		summary := appSummary(app)
		groups, people := map[string]bool{}, map[uuid.UUID]bool{}
		for _, m := range mappings {
			for _, g := range m.groups {
				groups[g.ID] = true
			}
			for _, person := range m.people {
				people[person.ID] = true
			}
		}
		summary.Groups, summary.Users = len(groups), len(people)
		out = append(out, summary)
	}
	return out, nil
}

// AppAssignments is GET /v1/apps/{clientId}/assignments: the direct
// mappings of the app's roles, Groups first by path, then people by name.
func (s *service) AppAssignments(ctx context.Context, p authz.Principal, clientID string, filter AppAssignmentFilter) (AppAssignments, error) {
	if err := s.allow(p, authz.TypeAppAssignment, authz.Read); err != nil {
		return AppAssignments{}, err
	}
	app, err := s.appOf(clientID)
	if err != nil {
		return AppAssignments{}, err
	}
	roles := app.Roles
	if filter.Role != "" {
		if !slices.Contains(app.Roles, filter.Role) {
			return AppAssignments{}, ErrInvalid
		}
		roles = []string{filter.Role}
	}
	if filter.Type != "" && filter.Type != AppTargetGroup && filter.Type != AppTargetUser {
		return AppAssignments{}, ErrInvalid
	}
	mappings, err := s.appMappings(ctx, app, roles)
	if err != nil {
		return AppAssignments{}, err
	}
	out := AppAssignments{App: appSummary(app), Assignments: []AppAssignment{}}
	out.AuditVisible = s.allow(p, authz.TypeTeamAudit, authz.Read) == nil
	latest, err := s.latestAppChanges(ctx, app.Client, out.AuditVisible)
	if err != nil {
		return AppAssignments{}, err
	}
	reads := newAppReads(s)
	groups, people := map[string]bool{}, map[uuid.UUID]bool{}
	for _, m := range mappings {
		if filter.Type != AppTargetUser {
			for _, g := range m.groups {
				groups[g.ID] = true
				out.Assignments = append(out.Assignments, AppAssignment{Type: AppTargetGroup, ID: g.ID, Role: m.role, Path: g.Path})
			}
		}
		if filter.Type != AppTargetGroup {
			for _, person := range m.people {
				people[person.ID] = true
				out.Assignments = append(out.Assignments, AppAssignment{Type: AppTargetUser, ID: person.ID.String(), Role: m.role})
			}
		}
	}
	out.App.Groups, out.App.Users = len(groups), len(people)
	accounts, err := s.users.Accounts(ctx, slices.Collect(maps.Keys(people)))
	if err != nil {
		return AppAssignments{}, err
	}
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(appStatsAtOnce)
	for i := range out.Assignments {
		row := &out.Assignments[i]
		g.Go(func() error { return reads.fill(gctx, row, accounts) })
	}
	if err := g.Wait(); err != nil {
		return AppAssignments{}, err
	}
	for i := range out.Assignments {
		latest.stamp(&out.Assignments[i])
	}
	order := func(role string) int { return slices.Index(app.Roles, role) }
	slices.SortStableFunc(out.Assignments, func(a, b AppAssignment) int {
		switch {
		case a.Type != b.Type:
			return strings.Compare(a.Type, b.Type) // group before user
		case a.Type == AppTargetGroup && a.Path != b.Path:
			return strings.Compare(a.Path, b.Path)
		case a.Type == AppTargetUser && a.Name != b.Name:
			return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
		case a.ID != b.ID:
			return strings.Compare(a.ID, b.ID)
		}
		return order(a.Role) - order(b.Role)
	})
	return out, nil
}

// appChanges are the latest app rows of a client by target and role.
type appChanges struct {
	rows  map[string]teamaudit.Row
	names map[uuid.UUID]string
}

// latestAppChanges reads them, when visible, with the actors' names.
func (s *service) latestAppChanges(ctx context.Context, client string, visible bool) (appChanges, error) {
	out := appChanges{rows: map[string]teamaudit.Row{}}
	if !visible || s.appAudit == nil {
		return out, nil
	}
	rows, err := s.appAudit.LatestAppRows(ctx, client)
	if err != nil {
		return appChanges{}, err
	}
	ids := make([]uuid.UUID, 0, len(rows))
	for _, r := range rows {
		out.rows[r.AppKey()] = r
		ids = append(ids, r.Actor.ID)
	}
	if out.names, err = s.accountNames(ctx, ids); err != nil {
		return appChanges{}, err
	}
	return out, nil
}

// stamp sets the row's AssignedBy and AssignedAt from its latest change:
// only an app.assign (a later app.unassign means the mapping was made
// again outside the panel).
func (c appChanges) stamp(row *AppAssignment) {
	key := teamaudit.Row{Record: teamaudit.Record{After: &teamaudit.State{Role: row.Role}}}
	if row.Type == AppTargetGroup {
		key.GroupID = row.ID
	} else if id, err := uuid.Parse(row.ID); err == nil {
		key.TargetUserID = id
	}
	r, ok := c.rows[key.AppKey()]
	if !ok || r.Action != teamaudit.ActionAppAssign {
		return
	}
	at := r.OccurredAt
	row.AssignedBy, row.AssignedAt = &AppActor{ID: r.Actor.ID, Name: c.names[r.Actor.ID]}, &at
}

// accountNames are core's names of the people ids: their profile names,
// "Silinmiş kullanıcı" for one core may no longer show, none for one core
// has no row of.
func (s *service) accountNames(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	out := map[uuid.UUID]string{}
	if len(ids) == 0 {
		return out, nil
	}
	accounts, err := s.users.Accounts(ctx, ids)
	if err != nil {
		return nil, err
	}
	for id, a := range accounts {
		switch {
		case a.Blocked:
			out[id] = user.DeletedDisplayName
		case a.Stored:
			out[id] = strings.TrimSpace(a.User.FirstName + " " + a.User.LastName)
		}
	}
	return out, nil
}

// appReads are the Group and person reads of one answer, each made once.
type appReads struct {
	s       *service
	mu      sync.Mutex
	groups  map[string]Group
	subs    map[string][]Group
	members map[string][]Person
	people  map[uuid.UUID]Person
}

func newAppReads(s *service) *appReads {
	return &appReads{s: s, groups: map[string]Group{}, subs: map[string][]Group{}, members: map[string][]Person{}, people: map[uuid.UUID]Person{}}
}

func memo[K comparable, V any](r *appReads, cache map[K]V, key K, read func() (V, error)) (V, error) {
	r.mu.Lock()
	v, ok := cache[key]
	r.mu.Unlock()
	if ok {
		return v, nil
	}
	v, err := read()
	if err != nil {
		return v, err
	}
	r.mu.Lock()
	cache[key] = v
	r.mu.Unlock()
	return v, nil
}

func (r *appReads) group(ctx context.Context, id string) (Group, error) {
	return memo(r, r.groups, id, func() (Group, error) { return r.s.dir.GetGroup(ctx, id) })
}

func (r *appReads) person(ctx context.Context, id uuid.UUID) (Person, error) {
	return memo(r, r.people, id, func() (Person, error) { return r.s.dir.GetUser(ctx, id) })
}

// stats are how many Groups are below g and how many enabled people g and
// they hold, each once; at most seatGroupLimit Groups are walked.
func (r *appReads) stats(ctx context.Context, g Group) (int, int, error) {
	queue, seen := []Group{g}, map[string]bool{g.ID: true}
	people := map[uuid.UUID]bool{}
	for len(queue) > 0 && len(seen) <= seatGroupLimit {
		next := queue[0]
		queue = queue[1:]
		members, err := memo(r, r.members, next.ID, func() ([]Person, error) { return r.s.dir.Members(ctx, next.ID) })
		if err != nil {
			return 0, 0, err
		}
		for _, person := range members {
			if person.Enabled {
				people[person.ID] = true
			}
		}
		subs, err := memo(r, r.subs, next.ID, func() ([]Group, error) { return r.s.dir.Subgroups(ctx, next.ID) })
		if err != nil {
			return 0, 0, err
		}
		for _, sub := range subs {
			if !seen[sub.ID] {
				seen[sub.ID] = true
				queue = append(queue, sub)
			}
		}
	}
	return len(seen) - 1, len(people), nil
}

// fill completes a row read from the role's mappings: the Group's name,
// counts and area, or the person's name, address and area.
func (r *appReads) fill(ctx context.Context, row *AppAssignment, accounts map[uuid.UUID]user.Account) error {
	if row.Type == AppTargetGroup {
		g, err := r.group(ctx, row.ID)
		if errors.Is(err, ErrNotFound) {
			// Gone since the mappings were read.
			row.Area, row.Name = AppAreaCode, row.Path
			return nil
		}
		if err != nil {
			return err
		}
		row.Path, row.Name = g.Path, displayName(g).TR
		if row.InheritsTo, row.Members, err = r.stats(ctx, g); err != nil {
			return err
		}
		row.Area = AppAreaPanel
		if row.Reason = authz.AppAssignmentTarget(g.Path, r.s.privilegedRoots, authz.DefaultPolicy().LeaderSubgroups); row.Reason != "" {
			row.Area = AppAreaCode
		}
		return nil
	}
	id, _ := uuid.Parse(row.ID)
	person, err := r.person(ctx, id)
	if errors.Is(err, ErrNotFound) {
		row.Area = AppAreaCode
		return nil
	}
	if err != nil {
		return err
	}
	row.Name = strings.TrimSpace(person.FirstName + " " + person.LastName)
	row.Email = person.Email
	if a, ok := accounts[id]; ok && a.Blocked {
		row.Name, row.Email = user.DeletedDisplayName, ""
	} else if ok && a.Stored {
		row.Name = strings.TrimSpace(a.User.FirstName + " " + a.User.LastName)
	}
	if row.Name == "" {
		row.Name = person.Username
	}
	row.Area = AppAreaPanel
	if person.ServiceAccount {
		row.Area, row.Reason = AppAreaCode, authz.PlacementServiceAccount
	}
	row.Members = 1
	return nil
}

// appChange is a checked change: its target, read from Keycloak, its role
// and the row it writes.
type appChange struct {
	role   ClientRole
	group  Group
	person Person
	record teamaudit.Record
}

// checkAppChange reads and checks one change (docs/app-assignments.md,
// "Rules"). A caller who may not read the assignments is refused with
// nothing read; any other refusal, from the target on, is a denied row.
func (s *service) checkAppChange(ctx context.Context, p authz.Principal, clientID string, in AppAssignmentInput, action teamaudit.Action) (appChange, error) {
	if err := s.allow(p, authz.TypeAppAssignment, authz.Read); err != nil {
		return appChange{}, err
	}
	in.Type, in.ID, in.Role, in.Note = strings.TrimSpace(in.Type), strings.TrimSpace(in.ID), strings.TrimSpace(in.Role), strings.TrimSpace(in.Note)
	if in.Role == "" || utf8.RuneCountInString(in.Note) > teamaudit.MaxNote || !utf8.ValidString(in.Note) {
		return appChange{}, ErrInvalid
	}
	id, err := uuid.Parse(in.ID)
	if err != nil {
		return appChange{}, ErrInvalid
	}
	app, err := s.appOf(clientID)
	if err != nil {
		return appChange{}, err
	}
	c := appChange{role: ClientRole{ClientID: app.Client, Role: in.Role}}
	after := &teamaudit.State{Client: app.Client, Role: in.Role, Note: in.Note}
	switch in.Type {
	case AppTargetGroup:
		if c.group, err = s.dir.GetGroup(ctx, id.String()); err != nil {
			return appChange{}, err
		}
		after.ID, after.Path = c.group.ID, c.group.Path
		c.record = teamaudit.Record{Action: action, GroupID: c.group.ID, GroupPath: c.group.Path, After: after}
	case AppTargetUser:
		if c.person, err = s.dir.GetUser(ctx, id); err != nil {
			return appChange{}, err
		}
		c.record = teamaudit.Record{Action: action, TargetUserID: id, After: after}
	default:
		return appChange{}, ErrInvalid
	}
	if s.allow(p, authz.TypeAppAssignment, authz.Update) != nil {
		s.denyApp(ctx, c.record, teamaudit.BasisNone, "forbidden")
		return appChange{}, ErrForbidden
	}
	refuse := func(err *RoleRuleError) (appChange, error) {
		s.denyApp(ctx, c.record, teamaudit.BasisPrivileged, err.Code)
		return appChange{}, err
	}
	if !slices.Contains(app.Roles, in.Role) {
		return refuse(placementRefused(authz.PlacementAdminRole))
	}
	if action == teamaudit.ActionAppAssign {
		var rule *RoleRuleError
		if err := s.noKeycloakAdminRole(ctx, []ClientRole{c.role}); errors.As(err, &rule) {
			return refuse(rule)
		} else if err != nil {
			return appChange{}, err
		}
	}
	if in.Type == AppTargetGroup {
		if reason := authz.AppAssignmentTarget(c.group.Path, s.privilegedRoots, authz.DefaultPolicy().LeaderSubgroups); reason != "" {
			return refuse(placementRefused(reason))
		}
		return c, nil
	}
	if c.person.ServiceAccount {
		return refuse(placementRefused(authz.PlacementServiceAccount))
	}
	if action == teamaudit.ActionAppAssign {
		active, err := s.assignable(ctx, c.person)
		if err != nil {
			return appChange{}, err
		}
		if !active {
			return refuse(placementRefused(authz.PlacementInactiveUser))
		}
	}
	return c, nil
}

func placementRefused(reason string) *RoleRuleError {
	return &RoleRuleError{Status: http.StatusConflict, Code: CodeRolePlacement, Reason: reason, Detail: authz.PlacementDetail(reason)}
}

// assignable reports whether a person may be given an app role: enabled in
// Keycloak, and an active account in core when core has a row of them (one
// who never signed in to core has none).
func (s *service) assignable(ctx context.Context, person Person) (bool, error) {
	if !person.Enabled || person.ID == user.DeletedSubject {
		return false, nil
	}
	shadow, err := s.users.Get(ctx, person.ID)
	if errors.Is(err, user.ErrNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return shadow.AccountState == user.AccountActive, nil
}

// denyApp writes the denied row of a refused change. A row that cannot be
// written changes nothing (the change is refused anyway) and is logged.
func (s *service) denyApp(ctx context.Context, r teamaudit.Record, basis teamaudit.Basis, code string) {
	if s.audit == nil {
		return
	}
	actor, ok := teamaudit.ActorFrom(ctx)
	if !ok {
		log.Printf("app assignments: denied %s not recorded: no actor", r.Action)
		return
	}
	actor.Basis, actor.ViaGroupID = basis, ""
	r.Actor = actor
	if err := s.audit.Deny(ctx, r, code); err != nil {
		log.Printf("app assignments: denied %s not recorded: %v", r.Action, err)
	}
}

// holds reports whether the change's target holds its role directly.
func (s *service) holds(ctx context.Context, c appChange) (bool, error) {
	var roles []ClientRole
	var err error
	if c.record.TargetUserID != uuid.Nil {
		roles, err = s.dir.UserExtraRoles(ctx, c.record.TargetUserID)
	} else {
		roles, err = s.dir.GroupClientRoles(ctx, c.group.ID)
	}
	if err != nil {
		return false, err
	}
	return slices.Contains(roles, c.role), nil
}

// writeApp makes the change with its row in the Team audit record: no row,
// no change. What core keeps of role holders is dropped even when the write
// fails, since it may have reached Keycloak.
func (s *service) writeApp(ctx context.Context, c appChange, add bool) error {
	if s.audit == nil {
		return teamaudit.ErrUnavailable
	}
	defer s.delegation.ForgetAll()
	defer s.seats.forget()
	return recordWrite(ctx, s.audit, log.Printf, c.record, func() (*teamaudit.State, error) {
		switch {
		case c.record.TargetUserID != uuid.Nil && add:
			return nil, s.dir.AddUserExtraRole(ctx, c.record.TargetUserID, c.role)
		case c.record.TargetUserID != uuid.Nil:
			return nil, s.dir.RemoveUserExtraRole(ctx, c.record.TargetUserID, c.role)
		case add:
			return nil, s.dir.AddGroupClientRole(ctx, c.group.ID, c.role)
		default:
			return nil, s.dir.RemoveGroupClientRole(ctx, c.group.ID, c.role)
		}
	})
}

// AssignApp is POST /v1/apps/{clientId}/assignments: the role to the
// target, and the row as the list shows it. created is false when the
// target held the role directly already: nothing changed, no row.
func (s *service) AssignApp(ctx context.Context, p authz.Principal, clientID string, in AppAssignmentInput) (AppAssignment, bool, error) {
	if err := s.allow(p, authz.TypeAppAssignment, authz.Read); err != nil {
		return AppAssignment{}, false, err
	}
	var c appChange
	created := false
	err := s.codes.Locked(ctx, func(ctx context.Context, _ teamcode.Records) error {
		var err error
		if c, err = s.checkAppChange(ctx, p, clientID, in, teamaudit.ActionAppAssign); err != nil {
			return err
		}
		held, err := s.holds(ctx, c)
		if err != nil || held {
			return err
		}
		created = true
		return s.writeApp(ctx, c, true)
	})
	if err != nil {
		return AppAssignment{}, false, err
	}
	row, err := s.appRow(ctx, p, c)
	return row, created, err
}

// UnassignApp is DELETE /v1/apps/{clientId}/assignments: the role off the
// target. A target that does not hold it directly is ErrAppNotAssigned,
// with no row.
func (s *service) UnassignApp(ctx context.Context, p authz.Principal, clientID string, in AppAssignmentInput) error {
	if err := s.allow(p, authz.TypeAppAssignment, authz.Read); err != nil {
		return err
	}
	return s.codes.Locked(ctx, func(ctx context.Context, _ teamcode.Records) error {
		c, err := s.checkAppChange(ctx, p, clientID, in, teamaudit.ActionAppUnassign)
		if err != nil {
			return err
		}
		held, err := s.holds(ctx, c)
		if err != nil {
			return err
		}
		if !held {
			return ErrAppNotAssigned
		}
		return s.writeApp(ctx, c, false)
	})
}

// appRow is the change's target as the assignment list shows it.
func (s *service) appRow(ctx context.Context, p authz.Principal, c appChange) (AppAssignment, error) {
	row := AppAssignment{Type: AppTargetGroup, ID: c.group.ID, Role: c.role.Role, Path: c.group.Path}
	var accounts map[uuid.UUID]user.Account
	if c.record.TargetUserID != uuid.Nil {
		row = AppAssignment{Type: AppTargetUser, ID: c.record.TargetUserID.String(), Role: c.role.Role}
		var err error
		if accounts, err = s.users.Accounts(ctx, []uuid.UUID{c.record.TargetUserID}); err != nil {
			return AppAssignment{}, err
		}
	}
	reads := newAppReads(s)
	if c.group.ID != "" {
		reads.groups[c.group.ID] = c.group
	} else {
		reads.people[c.person.ID] = c.person
	}
	if err := reads.fill(ctx, &row, accounts); err != nil {
		return AppAssignment{}, err
	}
	latest, err := s.latestAppChanges(ctx, c.role.ClientID, s.allow(p, authz.TypeTeamAudit, authz.Read) == nil)
	if err != nil {
		return AppAssignment{}, err
	}
	latest.stamp(&row)
	return row, nil
}

// AppAudit is GET /v1/apps/{clientId}/audit: the app's last hundred rows of
// the Team audit record, for a caller who reads the assignments and the
// record (audit:read).
func (s *service) AppAudit(ctx context.Context, p authz.Principal, clientID string) ([]AppAuditEntry, error) {
	if err := s.allow(p, authz.TypeAppAssignment, authz.Read); err != nil {
		return nil, err
	}
	if err := s.allow(p, authz.TypeTeamAudit, authz.Read); err != nil {
		return nil, err
	}
	app, err := s.appOf(clientID)
	if err != nil {
		return nil, err
	}
	out := []AppAuditEntry{}
	if s.appAudit == nil {
		return out, nil
	}
	rows, err := s.appAudit.AppRows(ctx, app.Client, 100)
	if err != nil {
		return nil, err
	}
	ids := []uuid.UUID{}
	for _, r := range rows {
		ids = append(ids, r.Actor.ID)
		if r.TargetUserID != uuid.Nil {
			ids = append(ids, r.TargetUserID)
		}
	}
	names, err := s.accountNames(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		e := AppAuditEntry{
			ID: r.ID, OccurredAt: r.OccurredAt, Action: string(r.Action), Outcome: string(r.Outcome), ErrorCode: r.ErrorCode,
			Actor: AppActor{ID: r.Actor.ID, Name: names[r.Actor.ID]}, Type: AppTargetGroup, TargetID: r.GroupID, Path: r.GroupPath,
		}
		if r.TargetUserID != uuid.Nil {
			e.Type, e.TargetID, e.Name = AppTargetUser, r.TargetUserID.String(), names[r.TargetUserID]
		}
		if r.After != nil {
			e.Role, e.Note = r.After.Role, r.After.Note
		}
		out = append(out, e)
	}
	return out, nil
}

// AppTargets is GET /v1/apps/{clientId}/targets: for a caller who changes
// the assignments, the Groups an app role may be assigned to whose path,
// code or Turkish name holds q (at most twenty, by path), or the people a
// search for q finds by MemberCandidates' rule (at least three letters, at
// most ten, the address masked unless q is the address), each with the
// app's roles it holds directly.
func (s *service) AppTargets(ctx context.Context, p authz.Principal, clientID, kind, q string) ([]AppTarget, error) {
	if err := s.allow(p, authz.TypeAppAssignment, authz.Update); err != nil {
		return nil, err
	}
	app, err := s.appOf(clientID)
	if err != nil {
		return nil, err
	}
	q = strings.TrimSpace(q)
	assigned := func(roles []ClientRole) []string {
		out := []string{}
		for _, role := range app.Roles {
			if slices.Contains(roles, ClientRole{ClientID: app.Client, Role: role}) {
				out = append(out, role)
			}
		}
		return out
	}
	switch kind {
	case AppTargetGroup:
		all, err := s.dir.ListGroups(ctx)
		if err != nil {
			return nil, err
		}
		needle := foldSearch(q)
		leaders := authz.DefaultPolicy().LeaderSubgroups
		found := []Group{}
		for _, g := range all {
			if authz.AppAssignmentTarget(g.Path, s.privilegedRoots, leaders) != "" {
				continue
			}
			if needle == "" || strings.Contains(foldSearch(g.Path), needle) || strings.Contains(foldSearch(displayName(g).TR), needle) {
				found = append(found, g)
			}
		}
		slices.SortFunc(found, func(a, b Group) int { return strings.Compare(a.Path, b.Path) })
		if len(found) > appGroupTargetLimit {
			found = found[:appGroupTargetLimit]
		}
		reads := newAppReads(s)
		out := make([]AppTarget, len(found))
		eg, gctx := errgroup.WithContext(ctx)
		eg.SetLimit(appStatsAtOnce)
		for i, g := range found {
			eg.Go(func() error {
				t := AppTarget{Type: AppTargetGroup, ID: g.ID, Path: g.Path, Name: displayName(g).TR}
				var err error
				if t.InheritsTo, t.Members, err = reads.stats(gctx, g); err != nil {
					return err
				}
				roles, err := s.dir.GroupClientRoles(gctx, g.ID)
				if err != nil {
					return err
				}
				t.AssignedRoles = assigned(roles)
				out[i] = t
				return nil
			})
		}
		if err := eg.Wait(); err != nil {
			return nil, err
		}
		return out, nil
	case AppTargetUser:
		if utf8.RuneCountInString(q) < candidateQueryMin {
			return nil, ErrInvalid
		}
		shown, masked, err := s.candidateUsers(ctx, q)
		if err != nil {
			return nil, err
		}
		out := make([]AppTarget, 0, len(shown))
		for _, u := range shown {
			t := AppTarget{Type: AppTargetUser, ID: u.ID.String(), Name: strings.TrimSpace(u.FirstName + " " + u.LastName), Email: u.Email, Members: 1}
			if masked {
				t.Email = MaskEmail(u.Email)
			}
			roles, err := s.dir.UserExtraRoles(ctx, u.ID)
			if err != nil && !errors.Is(err, ErrNotFound) {
				return nil, err
			}
			t.AssignedRoles = assigned(roles)
			out = append(out, t)
		}
		return out, nil
	}
	return nil, ErrInvalid
}
