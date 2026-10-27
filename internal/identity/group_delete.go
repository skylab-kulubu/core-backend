package identity

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
	"github.com/skylab-kulubu/core-backend/internal/teamcode"
)

// GroupDeleteError describes a refused deletion. Members and Children
// count, for not_empty, the people in the Group and every Group under it
// and the Groups under it; Records are, for code_in_use, the kinds of
// core record that name a code to give up (teamcode.RecordEvent, …).
type GroupDeleteError struct {
	Status   int
	Code     string
	Detail   string
	Members  int
	Children int
	Records  []string
}

func (e *GroupDeleteError) Error() string {
	return fmt.Sprintf("identity: group deletion refused: %s", e.Code)
}
func deleteRefused(status int, code, detail string) error {
	return &GroupDeleteError{Status: status, Code: code, Detail: detail}
}

// DeleteGroup deletes the Group groupID (a Keycloak id; a name or path is
// never a target). Privileged deletes an empty leaf anywhere the guards
// allow, and a Team with every Group under it once the whole subtree is
// empty: no member (its Leader groups' included), no role mapping, no
// code core's records name. A Leader (delegation on) deletes so a Sub-team
// they manage strictly (authz.Policy.ManagesStrict: never their own Team,
// never a top Team) (ADR 0065 ticket 05).
func (s *service) DeleteGroup(ctx context.Context, p authz.Principal, groupID string) error {
	if err := s.allow(p, authz.TypeGroup, authz.Delete); err != nil {
		return s.delegatedDeleteGroup(ctx, p, groupID)
	}
	g, _, err := s.dir.GroupNode(ctx, groupID)
	if err != nil {
		return err
	}
	if g.ID != groupID || groupID == "" {
		return ErrNotFound
	}
	return s.deleteGroup(ctx, g, nil)
}

// delegatedDeleteGroup is DeleteGroup for a Leader: decided as every
// delegated write, outside the lock first, then again inside each of the
// deletion's two turns. The team.delete row names the Team that grants it.
func (s *service) delegatedDeleteGroup(ctx context.Context, p authz.Principal, groupID string) error {
	return s.delegatedTurns(ctx, p, teamaudit.ActionTeamDelete, groupID, uuid.Nil, authz.Policy.ManagesStrict,
		func(ctx context.Context, d authz.ManageDecision, read delegatedReader) error {
			_, _, g, err := read(ctx)
			if err != nil {
				return err
			}
			recheck := func(ctx context.Context) error {
				m, target, _, err := read(ctx)
				if err != nil {
					return err
				}
				if d := s.delegation.policy.ManagesStrict(m, target); !d.Allowed {
					s.deny(ctx, teamaudit.ActionTeamDelete, target.Group, uuid.Nil, d, d.Refusal)
					return decisionError(d)
				}
				return nil
			}
			return s.deleteGroup(asLeader(ctx, d), g, recheck)
		})
}

// deleteGroup deletes g, opening its team.delete row first. recheck, when
// not nil, runs first in each turn under the lock (a Leader's decision).
func (s *service) deleteGroup(ctx context.Context, g Group, recheck func(context.Context) error) error {
	if audited, ok := s.dir.(*auditedDirectory); ok {
		return audited.deleteGroupWith(ctx, g, func() error {
			return s.deleteEmptyGroup(ctx, g, audited.Directory, recheck)
		})
	}
	return s.deleteEmptyGroup(ctx, g, s.dir, recheck)
}

func (s *service) deleteEmptyGroup(ctx context.Context, original Group, directory Directory, recheck func(context.Context) error) error {
	if recheck == nil {
		recheck = func(context.Context) error { return nil }
	}
	// Commit the tombstones BEFORE Keycloak. A refused or uncertain delete
	// leaves the codes reserved; the same id can still be retried.
	var retired []string
	if err := s.codes.Locked(ctx, func(ctx context.Context, records teamcode.Records) error {
		if err := recheck(ctx); err != nil {
			return err
		}
		g, codes, err := s.checkGroupDelete(ctx, original, records)
		if err != nil {
			return err
		}
		for _, code := range codes {
			if err := records.Retire(ctx, code, g.ID, teamcode.ReasonDeleted); err != nil {
				return err
			}
		}
		retired = codes
		return nil
	}); err != nil {
		return err
	}
	return s.codes.Locked(ctx, func(ctx context.Context, records teamcode.Records) error {
		if err := recheck(ctx); err != nil {
			return err
		}
		g, codes, err := s.checkGroupDelete(ctx, original, records)
		if err != nil {
			return err
		}
		// A Group added under it between the turns has no retired code.
		if !slices.Equal(codes, retired) {
			return deleteRefused(http.StatusConflict, CodeGroupTreeMismatch, "The Groups under it changed; reload it before deleting.")
		}
		// Forgotten on an uncertain response too: Keycloak may have deleted it.
		defer s.forgetAllGroups()
		return directory.DeleteGroup(ctx, g.ID)
	})
}

// checkGroupDelete reads original again and checks every guard; it
// answers the Group and the codes its deletion gives up (its own and its
// Sub-teams', reserved names aside), in path order.
func (s *service) checkGroupDelete(ctx context.Context, original Group, records teamcode.Records) (Group, []string, error) {
	g, parent, err := s.dir.GroupNode(ctx, original.ID)
	if err != nil {
		return Group{}, nil, err
	}
	if g.ID != original.ID || g.Name != original.Name || g.Path != original.Path {
		return Group{}, nil, deleteRefused(http.StatusConflict, CodeGroupTreeMismatch, "The Group changed; reload it before deleting.")
	}
	// Roots, their ancestors and descendants are never deleted here. The
	// role tree, areas and reserved/Leader names are also structural Groups.
	for _, root := range s.privilegedRoots {
		path, root := strings.ToUpper(g.Path), strings.ToUpper(root)
		if path == root || strings.HasPrefix(path, root+"/") || strings.HasPrefix(root, path+"/") {
			return Group{}, nil, deleteRefused(http.StatusConflict, teamcode.PrivilegedRoot, "Privileged roots, their ancestors and descendants cannot be deleted here.")
		}
	}
	path := strings.ToUpper(g.Path)
	if s.names.Reserved(g.Name) || path == authz.RolesRoot || strings.HasPrefix(path, authz.RolesRoot+"/") ||
		(strings.HasPrefix(path, "/UYELER/") && strings.Count(path, "/") < 3) {
		return Group{}, nil, deleteRefused(http.StatusForbidden, "group_delete_protected", "Structural, role and Leader Groups cannot be deleted here.")
	}
	if err := s.deleteProtectionCanary(ctx); err != nil {
		return Group{}, nil, err
	}
	seen := map[string]bool{}
	node, next := g, parent
	for depth := 0; ; depth++ {
		if depth >= maxGroupDepth || seen[node.ID] || node.ID == "" || node.Name == "" || strings.Contains(node.Name, "/") {
			return Group{}, nil, deleteRefused(http.StatusConflict, CodeGroupTreeMismatch, "The Group tree cannot be verified.")
		}
		seen[node.ID] = true
		// /UYELER is the normal ancestor of Teams. Other structural
		// containers are never pruned here.
		if s.names.Structural(node.Name) && node.Path != "/UYELER" {
			return Group{}, nil, deleteRefused(http.StatusForbidden, "group_delete_protected", "A system or Leader Group contains this Group.")
		}
		protection, err := s.dir.GroupProtection(ctx, node.ID)
		if err != nil {
			return Group{}, nil, deleteRefused(http.StatusServiceUnavailable, CodeProtectionUnverified, "Role protection cannot be read.")
		}
		if protection.Protected {
			return Group{}, nil, deleteRefused(http.StatusForbidden, authz.RefusalProtectedGroup, "The Group or an ancestor holds protected roles.")
		}
		if next == "" {
			if node.Path != "/"+node.Name {
				return Group{}, nil, deleteRefused(http.StatusConflict, CodeGroupTreeMismatch, "The Group tree cannot be verified.")
			}
			break
		}
		above, aboveParent, err := s.dir.GroupNode(ctx, next)
		if err != nil {
			return Group{}, nil, err
		}
		if above.ID != next || node.Path != above.Path+"/"+node.Name {
			return Group{}, nil, deleteRefused(http.StatusConflict, CodeGroupTreeMismatch, "The Group tree cannot be verified.")
		}
		node, next = above, aboveParent
	}
	roles, err := s.dir.GroupClientRoles(ctx, g.ID)
	if err != nil {
		return Group{}, nil, deleteRefused(http.StatusServiceUnavailable, CodeProtectionUnverified, "Client role mappings cannot be read.")
	}
	if len(roles) != 0 {
		return Group{}, nil, deleteRefused(http.StatusForbidden, "group_delete_protected", "Remove the Group's role mappings before deleting it.")
	}
	if !s.isTeam(g.Path) {
		children, err := s.dir.Subgroups(ctx, g.ID)
		if err != nil {
			return Group{}, nil, err
		}
		members, err := s.dir.Members(ctx, g.ID)
		if err != nil {
			return Group{}, nil, err
		}
		if len(children) != 0 || len(members) != 0 {
			return Group{}, nil, &GroupDeleteError{Status: http.StatusConflict, Code: "not_empty", Detail: "Move or remove the members and subgroups first.", Members: len(members), Children: len(children)}
		}
		kinds, err := records.References(ctx, g.Name)
		if err != nil {
			return Group{}, nil, err
		}
		if len(kinds) > 0 {
			return Group{}, nil, &GroupDeleteError{Status: http.StatusConflict, Code: "code_in_use", Detail: "Core records still reference this Group's code.", Records: kinds}
		}
		return g, []string{g.Name}, nil
	}
	codes, err := s.checkTeamSubtree(ctx, g, records)
	if err != nil {
		return Group{}, nil, err
	}
	return g, codes, nil
}

// checkTeamSubtree checks that the Team g and every Group under it may go
// with it: only Sub-teams and Leader groups under it, none mapping a role
// or protected, nobody in any of them, and no code of theirs named by
// core's records. It answers the codes to give up, g's first.
func (s *service) checkTeamSubtree(ctx context.Context, g Group, records teamcode.Records) ([]string, error) {
	below, err := s.groupsBelow(ctx, g)
	var refused *DelegationError
	if errors.As(err, &refused) {
		return nil, deleteRefused(http.StatusConflict, CodeGroupTreeMismatch, "The Groups under it cannot be verified.")
	}
	if err != nil {
		return nil, err
	}
	members := 0
	codes := []string{g.Name}
	for _, group := range append([]Group{g}, below...) {
		if group.ID != g.ID {
			if _, _, ok := authz.TeamOf(group.Path, s.privilegedRoots, authz.DefaultPolicy().LeaderSubgroups); !ok {
				return nil, deleteRefused(http.StatusForbidden, "group_delete_protected", "Only Sub-teams and Leader groups may be deleted with a Team.")
			}
			protection, err := s.dir.GroupProtection(ctx, group.ID)
			if err != nil {
				return nil, deleteRefused(http.StatusServiceUnavailable, CodeProtectionUnverified, "Role protection cannot be read.")
			}
			if protection.Protected {
				return nil, deleteRefused(http.StatusForbidden, authz.RefusalProtectedGroup, "A Group under it holds protected roles.")
			}
			roles, err := s.dir.GroupClientRoles(ctx, group.ID)
			if err != nil {
				return nil, deleteRefused(http.StatusServiceUnavailable, CodeProtectionUnverified, "Client role mappings cannot be read.")
			}
			if len(roles) != 0 {
				return nil, deleteRefused(http.StatusForbidden, "group_delete_protected", "Remove the role mappings of the Groups under it before deleting it.")
			}
			if !s.names.Reserved(group.Name) {
				codes = append(codes, group.Name)
			}
		}
		people, err := s.dir.Members(ctx, group.ID)
		if err != nil {
			return nil, err
		}
		members += len(people)
	}
	if members != 0 {
		return nil, &GroupDeleteError{Status: http.StatusConflict, Code: "not_empty",
			Detail: "Remove the members of the Team and of every Group under it (its Leaders' too) first.", Members: members, Children: len(below)}
	}
	var kinds []string
	for _, code := range codes {
		held, err := records.References(ctx, code)
		if err != nil {
			return nil, err
		}
		for _, kind := range held {
			if !slices.Contains(kinds, kind) {
				kinds = append(kinds, kind)
			}
		}
	}
	if len(kinds) > 0 {
		return nil, &GroupDeleteError{Status: http.StatusConflict, Code: "code_in_use", Records: kinds,
			Detail: "Core's records (" + strings.Join(kinds, ", ") + ") still name the code of the Team or of a Group under it."}
	}
	return codes, nil
}

// Keycloak may hide role mappings without view-clients rather than refuse
// them. As in TeamDelegation.Canary, an existing Privileged root must prove
// role visibility. Do not use the read cache for a destructive check.
func (s *service) deleteProtectionCanary(ctx context.Context) error {
	for _, root := range s.privilegedRoots {
		g, err := s.dir.GetGroup(ctx, root)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if g.Path != root || g.ID == "" {
			continue
		}
		got, err := s.dir.GroupProtection(ctx, g.ID)
		if err != nil {
			return deleteRefused(http.StatusServiceUnavailable, CodeProtectionUnverified, "Role protection cannot be read.")
		}
		if !got.Protected {
			continue
		}
		// A realm role can make GroupProtection true while Keycloak hides
		// every client mapping. Require a visible known admin client-role
		// pair too; realm-only protection is not a view-clients canary.
		roles, err := s.dir.GroupClientRoles(ctx, g.ID)
		if err != nil {
			return deleteRefused(http.StatusServiceUnavailable, CodeProtectionUnverified, "Client role mappings cannot be read.")
		}
		for _, role := range roles {
			if authz.ClassifyRole(role.ClientID, role.Role) == authz.RoleClassAdmin {
				return nil
			}
		}
	}
	return deleteRefused(http.StatusServiceUnavailable, CodeProtectionUnverified, "Role protection cannot be verified.")
}
