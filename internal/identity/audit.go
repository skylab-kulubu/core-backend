package identity

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"

	"github.com/Nerzal/gocloak/v13"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
)

// AuditedDirectory is dir with its Group writes in the Team audit record
// (docs/team-audit.md): a create, a rename or change of attributes, a member
// added or removed and a Group's role mappings. Each write reads what the
// row needs (the Group's path, its old name, attributes or roles), opens a
// pending row, asks dir, and writes the outcome. A row that cannot be opened
// stops the write before dir is asked: the error is teamaudit.ErrUnavailable
// (503 audit_unavailable). The actor is the one ctx carries
// (teamaudit.WithActor); a write without one is refused the same way.
//
// A failure that does not tell whether Keycloak made the change, and an
// outcome that cannot be written, leave the row pending ("outcome unknown")
// and are logged; the write's own result stands. Reads pass through.
func AuditedDirectory(dir Directory, audit teamaudit.Writer, logf func(string, ...any)) Directory {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &auditedDirectory{Directory: dir, audit: audit, logf: logf}
}

type auditedDirectory struct {
	Directory
	audit teamaudit.Writer
	logf  func(string, ...any)
}

// write records do as r: a pending row, then do, then its outcome. do's
// State, when not nil, is the row's after. A failure that does not tell
// whether Keycloak made the change (a timeout, a request given up, a lost
// connection, a 5xx) leaves the row pending: its outcome is unknown.
func (d *auditedDirectory) write(ctx context.Context, r teamaudit.Record, do func() (*teamaudit.State, error)) error {
	actor, ok := teamaudit.ActorFrom(ctx)
	if !ok {
		return teamaudit.ErrUnavailable
	}
	r.Actor = actor
	id, err := d.audit.Begin(ctx, r)
	if err != nil {
		return err
	}
	after, err := do()
	outcome, code := teamaudit.OutcomeOK, ""
	if err != nil {
		var refused bool
		if code, refused = refusal(err); !refused {
			d.logf("team audit: record %s stays pending: outcome unknown (%s)", id, code)
			return err
		}
		outcome = teamaudit.OutcomeFailed
	}
	if finishErr := d.audit.Finish(ctx, id, outcome, code, after); finishErr != nil {
		d.logf("team audit: record %s stays pending: %v", id, finishErr)
	}
	return err
}

// refusal is the code a failed write's row keeps, and whether the failure
// says Keycloak made no change: it answered and refused (a 4xx, an unknown
// Group or person, an invalid write). Anything else (a timeout, a request
// given up, a lost connection, a 5xx) may have come after Keycloak made the
// change, so its outcome is unknown.
func refusal(err error) (string, bool) {
	switch {
	case errors.Is(err, ErrNotFound):
		return "not_found", true
	case errors.Is(err, ErrForbidden):
		return "forbidden", true
	case errors.Is(err, ErrInvalid):
		return "invalid", true
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout", false
	case errors.Is(err, context.Canceled):
		return "canceled", false
	}
	status := 0
	var api *gocloak.APIError
	var value gocloak.APIError
	if errors.As(err, &api) && api != nil {
		status = api.Code
	} else if errors.As(err, &value) {
		status = value.Code
	}
	if status >= 400 && status < 500 {
		return "keycloak_" + strconv.Itoa(status), true
	}
	if status != 0 {
		return "keycloak_" + strconv.Itoa(status), false
	}
	return "directory_error", false
}

func (d *auditedDirectory) CreateGroup(ctx context.Context, parentRef, name string) (Group, error) {
	r := teamaudit.Record{Action: teamaudit.ActionTeamCreate, GroupPath: "/" + name}
	after := &teamaudit.State{Code: name}
	if parentRef != "" {
		parent, err := d.Directory.GetGroup(ctx, parentRef)
		if err != nil {
			return Group{}, err
		}
		r.GroupPath = parent.Path + "/" + name
		after.ParentID = parent.ID
	}
	after.Path = r.GroupPath
	r.After = after
	var created Group
	err := d.write(ctx, r, func() (*teamaudit.State, error) {
		var err error
		if created, err = d.Directory.CreateGroup(ctx, parentRef, name); err != nil {
			return nil, err
		}
		return &teamaudit.State{ID: created.ID, Path: created.Path, ParentID: after.ParentID, Code: created.Name}, nil
	})
	return created, err
}

// UpdateGroup records a rename as team.code and anything else as
// group.attributes, with the name and the attributes that differ.
func (d *auditedDirectory) UpdateGroup(ctx context.Context, g Group) (Group, error) {
	ref := g.ID
	if ref == "" {
		ref = g.Path
	}
	old, err := d.Directory.GetGroup(ctx, ref)
	if err != nil {
		return Group{}, err
	}
	name, attributes := old.Name, old.Attributes
	if g.Name != "" {
		name = g.Name
	}
	if g.Attributes != nil {
		attributes = g.Attributes
	}
	before := &teamaudit.State{ID: old.ID, Path: old.Path, Code: old.Name}
	after := &teamaudit.State{ID: old.ID, Path: old.Path[:strings.LastIndex(old.Path, "/")+1] + name, Code: name}
	before.Attributes, after.Attributes, before.OtherAttributes = teamaudit.AttributeChange(old.Attributes, attributes)
	after.OtherAttributes = before.OtherAttributes
	action := teamaudit.ActionGroupAttributes
	if name != old.Name {
		action = teamaudit.ActionTeamCode
	}
	r := teamaudit.Record{Action: action, GroupID: old.ID, GroupPath: old.Path, Before: before, After: after}
	var updated Group
	err = d.write(ctx, r, func() (*teamaudit.State, error) {
		var err error
		updated, err = d.Directory.UpdateGroup(ctx, g)
		return nil, err
	})
	return updated, err
}

func (d *auditedDirectory) AddMember(ctx context.Context, groupID string, userID uuid.UUID) error {
	return d.member(ctx, teamaudit.ActionMemberAdd, groupID, userID, d.Directory.AddMember)
}

// RemoveMember records the Group the membership is removed from: through a
// parent roster, the Source group the person sits in (the service passes
// that one).
func (d *auditedDirectory) RemoveMember(ctx context.Context, groupID string, userID uuid.UUID) error {
	return d.member(ctx, teamaudit.ActionMemberRemove, groupID, userID, d.Directory.RemoveMember)
}

func (d *auditedDirectory) member(ctx context.Context, action teamaudit.Action, groupID string, userID uuid.UUID,
	do func(context.Context, string, uuid.UUID) error) error {
	g, err := d.Directory.GetGroup(ctx, groupID)
	if err != nil {
		return err
	}
	r := teamaudit.Record{Action: action, GroupID: g.ID, GroupPath: g.Path, TargetUserID: userID}
	return d.write(ctx, r, func() (*teamaudit.State, error) {
		return nil, do(ctx, groupID, userID)
	})
}

// SetGroupClientRoles records the Group's role mappings before and after.
func (d *auditedDirectory) SetGroupClientRoles(ctx context.Context, groupID string, roles []ClientRole) error {
	g, err := d.Directory.GetGroup(ctx, groupID)
	if err != nil {
		return err
	}
	old, err := d.Directory.GroupClientRoles(ctx, g.ID)
	if err != nil {
		return err
	}
	r := teamaudit.Record{
		Action: teamaudit.ActionGroupRoles, GroupID: g.ID, GroupPath: g.Path,
		Before: &teamaudit.State{ID: g.ID, Path: g.Path, Roles: roleNames(old)},
		After:  &teamaudit.State{ID: g.ID, Path: g.Path, Roles: roleNames(roles)},
	}
	return d.write(ctx, r, func() (*teamaudit.State, error) {
		return nil, d.Directory.SetGroupClientRoles(ctx, groupID, roles)
	})
}

// roleNames are roles as "client:role", sorted. A State without roles is a
// Group without role mappings.
func roleNames(roles []ClientRole) []string {
	names := make([]string, 0, len(roles))
	for _, role := range roles {
		names = append(names, role.ClientID+":"+role.Role)
	}
	slices.Sort(names)
	return names
}
