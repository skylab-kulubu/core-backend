package identity

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/skylab-kulubu/core-backend/internal/authz"
)

// GroupGetter reads one Group by id or path.
type GroupGetter interface {
	GetGroup(ctx context.Context, idOrPath string) (Group, error)
}

// PrivilegedRootsLine is the startup line of the Privileged roots
// (authz.PrivilegedRootsEnv, docs/authz-roles.md): each root looked up on
// Keycloak by its path, the ones found with their Group ids and the missing
// ones, in one line. Paths and ids only, no person. It never stops core: a
// missing root makes nobody Privileged through it until a Group is created
// at its path, and core decides by the configured paths whatever Keycloak
// answers.
func PrivilegedRootsLine(ctx context.Context, dir GroupGetter, roots []string) string {
	var found, missing []string
	for _, root := range roots {
		g, err := dir.GetGroup(ctx, root)
		switch {
		case errors.Is(err, ErrNotFound) || (err == nil && g.Path != root):
			missing = append(missing, root)
		case err != nil:
			return fmt.Sprintf("authz privileged roots (%s) could not be checked on Keycloak: %v", authz.PrivilegedRootsEnv, err)
		default:
			found = append(found, root+" ("+g.ID+")")
		}
	}
	line := fmt.Sprintf("authz privileged roots (%s) on Keycloak: found %s; missing %s",
		authz.PrivilegedRootsEnv, orNone(found), orNone(missing))
	if len(missing) > 0 {
		line += "; nobody is Privileged through a missing root until a Group is created at its path"
	}
	return line
}

func orNone(list []string) string {
	if len(list) == 0 {
		return "none"
	}
	return strings.Join(list, ", ")
}
