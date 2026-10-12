package authz

import (
	"fmt"
	"strings"
)

// PrivilegedRootsEnv names the Privileged roots.
const PrivilegedRootsEnv = "AUTHZ_PRIVILEGED_ROOTS"

// DefaultPrivilegedRoots are the six Group paths Keycloak maps core's
// roles to (ADR-0059): ADMIN, YK and DK at the realm's top and under
// /UYELER.
func DefaultPrivilegedRoots() []string {
	return []string{"/ADMIN", "/YK", "/DK", "/UYELER/ADMIN", "/UYELER/YK", "/UYELER/DK"}
}

// PrivilegedRootsFromEnv reads PrivilegedRootsEnv: Group paths separated by
// commas, blanks around each ignored. Unset or blank is
// DefaultPrivilegedRoots. Each path starts with "/", does not end with one,
// has no empty segment and is named once; anything else is an error that
// names the variable, so a typo cannot quietly change who is Privileged.
func PrivilegedRootsFromEnv(getenv func(string) string) ([]string, error) {
	raw := strings.TrimSpace(getenv(PrivilegedRootsEnv))
	if raw == "" {
		return DefaultPrivilegedRoots(), nil
	}
	var roots []string
	seen := map[string]bool{}
	for _, entry := range strings.Split(raw, ",") {
		root := strings.TrimSpace(entry)
		switch {
		case root == "":
			return nil, fmt.Errorf("%s: an empty path in %q", PrivilegedRootsEnv, raw)
		case !strings.HasPrefix(root, "/"):
			return nil, fmt.Errorf("%s: %q does not start with /", PrivilegedRootsEnv, root)
		case strings.HasSuffix(root, "/"):
			return nil, fmt.Errorf("%s: %q ends with /", PrivilegedRootsEnv, root)
		case strings.Contains(root, "//"):
			return nil, fmt.Errorf("%s: %q has an empty segment", PrivilegedRootsEnv, root)
		case seen[root]:
			return nil, fmt.Errorf("%s: %q is named twice", PrivilegedRootsEnv, root)
		}
		seen[root] = true
		roots = append(roots, root)
	}
	return roots, nil
}

// PrivilegedRoot is the root of roots that path is, or is under, or "". The
// match is anchored at the root: a Group of the same name elsewhere in the
// tree (/UYELER/ARGE/WEBLAB/ADMIN) is under no root, nor is one whose name
// only starts with the root's (/YKX).
func PrivilegedRoot(roots []string, path string) string {
	for _, root := range roots {
		if path == root || strings.HasPrefix(path, root+"/") {
			return root
		}
	}
	return ""
}

// RootLabel is how logs name a Privileged root: its last segment (ADMIN, YK
// or DK for the default roots), never the path.
func RootLabel(root string) string {
	return root[strings.LastIndex(root, "/")+1:]
}
