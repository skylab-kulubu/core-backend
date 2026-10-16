package identity

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/teamcode"
)

// GroupDeleteError describes a refused leaf deletion, never a cascading delete.
type GroupDeleteError struct {
	Status   int
	Code     string
	Detail   string
	Members  int
	Children int
}

func (e *GroupDeleteError) Error() string {
	return fmt.Sprintf("identity: group deletion refused: %s", e.Code)
}
func deleteRefused(status int, code, detail string) error {
	return &GroupDeleteError{Status: status, Code: code, Detail: detail}
}

// DeleteGroup is the Privileged, empty-leaf route. Delegated subtree deletion
// is a separate feature (ADR 0065 ticket 05). A name or path is never a target.
func (s *service) DeleteGroup(ctx context.Context, p authz.Principal, groupID string) error {
	if err := s.allow(p, authz.TypeGroup, authz.Delete); err != nil {
		return err
	}
	g, _, err := s.dir.GroupNode(ctx, groupID)
	if err != nil {
		return err
	}
	if g.ID != groupID || groupID == "" {
		return ErrNotFound
	}
	if audited, ok := s.dir.(*auditedDirectory); ok {
		return audited.deleteGroupWith(ctx, g, func() error {
			return s.deleteEmptyGroup(ctx, g, audited.Directory)
		})
	}
	return s.deleteEmptyGroup(ctx, g, s.dir)
}

func (s *service) deleteEmptyGroup(ctx context.Context, original Group, directory Directory) error {
	// Commit the tombstone BEFORE Keycloak. A refused or uncertain delete
	// leaves the Group's code reserved; the same id can still be retried.
	if err := s.codes.Locked(ctx, func(ctx context.Context, records teamcode.Records) error {
		g, err := s.checkGroupDelete(ctx, original, records)
		if err != nil {
			return err
		}
		return records.Retire(ctx, g.Name, g.ID, teamcode.ReasonDeleted)
	}); err != nil {
		return err
	}
	return s.codes.Locked(ctx, func(ctx context.Context, records teamcode.Records) error {
		g, err := s.checkGroupDelete(ctx, original, records)
		if err != nil {
			return err
		}
		// Forgotten on an uncertain response too: Keycloak may have deleted it.
		defer s.forgetAllGroups()
		return directory.DeleteGroup(ctx, g.ID)
	})
}

func (s *service) checkGroupDelete(ctx context.Context, original Group, records teamcode.Records) (Group, error) {
	g, parent, err := s.dir.GroupNode(ctx, original.ID)
	if err != nil {
		return Group{}, err
	}
	if g.ID != original.ID || g.Name != original.Name || g.Path != original.Path {
		return Group{}, deleteRefused(http.StatusConflict, CodeGroupTreeMismatch, "The Group changed; reload it before deleting.")
	}
	// Roots, their ancestors and descendants are never deleted here. The
	// role tree, areas and reserved/Leader names are also structural Groups.
	for _, root := range s.privilegedRoots {
		path, root := strings.ToUpper(g.Path), strings.ToUpper(root)
		if path == root || strings.HasPrefix(path, root+"/") || strings.HasPrefix(root, path+"/") {
			return Group{}, deleteRefused(http.StatusConflict, teamcode.PrivilegedRoot, "Privileged roots, their ancestors and descendants cannot be deleted here.")
		}
	}
	path := strings.ToUpper(g.Path)
	if s.names.Reserved(g.Name) || path == authz.RolesRoot || strings.HasPrefix(path, authz.RolesRoot+"/") ||
		(strings.HasPrefix(path, "/UYELER/") && strings.Count(path, "/") < 3) {
		return Group{}, deleteRefused(http.StatusForbidden, "group_delete_protected", "Structural, role and Leader Groups cannot be deleted here.")
	}
	if err := s.deleteProtectionCanary(ctx); err != nil {
		return Group{}, err
	}
	seen := map[string]bool{}
	node, next := g, parent
	for depth := 0; ; depth++ {
		if depth >= maxGroupDepth || seen[node.ID] || node.ID == "" || node.Name == "" || strings.Contains(node.Name, "/") {
			return Group{}, deleteRefused(http.StatusConflict, CodeGroupTreeMismatch, "The Group tree cannot be verified.")
		}
		seen[node.ID] = true
		// /UYELER is the normal ancestor of Teams. Other structural
		// containers are never pruned here.
		if s.names.Structural(node.Name) && node.Path != "/UYELER" {
			return Group{}, deleteRefused(http.StatusForbidden, "group_delete_protected", "A system or Leader Group contains this Group.")
		}
		protection, err := s.dir.GroupProtection(ctx, node.ID)
		if err != nil {
			return Group{}, deleteRefused(http.StatusServiceUnavailable, CodeProtectionUnverified, "Role protection cannot be read.")
		}
		if protection.Protected {
			return Group{}, deleteRefused(http.StatusForbidden, authz.RefusalProtectedGroup, "The Group or an ancestor holds protected roles.")
		}
		if next == "" {
			if node.Path != "/"+node.Name {
				return Group{}, deleteRefused(http.StatusConflict, CodeGroupTreeMismatch, "The Group tree cannot be verified.")
			}
			break
		}
		above, aboveParent, err := s.dir.GroupNode(ctx, next)
		if err != nil {
			return Group{}, err
		}
		if above.ID != next || node.Path != above.Path+"/"+node.Name {
			return Group{}, deleteRefused(http.StatusConflict, CodeGroupTreeMismatch, "The Group tree cannot be verified.")
		}
		node, next = above, aboveParent
	}
	roles, err := s.dir.GroupClientRoles(ctx, g.ID)
	if err != nil {
		return Group{}, deleteRefused(http.StatusServiceUnavailable, CodeProtectionUnverified, "Client role mappings cannot be read.")
	}
	if len(roles) != 0 {
		return Group{}, deleteRefused(http.StatusForbidden, "group_delete_protected", "Remove the Group's role mappings before deleting it.")
	}
	children, err := s.dir.Subgroups(ctx, g.ID)
	if err != nil {
		return Group{}, err
	}
	members, err := s.dir.Members(ctx, g.ID)
	if err != nil {
		return Group{}, err
	}
	if len(children) != 0 || len(members) != 0 {
		return Group{}, &GroupDeleteError{Status: http.StatusConflict, Code: "not_empty", Detail: "Move or remove the members and subgroups first.", Members: len(members), Children: len(children)}
	}
	used, err := records.Referenced(ctx, g.Name)
	if err != nil {
		return Group{}, err
	}
	if used {
		return Group{}, deleteRefused(http.StatusConflict, "code_in_use", "Core records still reference this Group's code.")
	}
	return g, nil
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
