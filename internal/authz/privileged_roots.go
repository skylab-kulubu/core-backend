package authz

import (
	"fmt"
	"slices"
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

// PrivilegedReaderRootsEnv names the Privileged reader roots.
const PrivilegedReaderRootsEnv = "AUTHZ_PRIVILEGED_READER_ROOTS"

// DefaultPrivilegedReaderRoots are DK's two roots (grup-yonetimi GY1: DK
// reads everything and changes nothing).
func DefaultPrivilegedReaderRoots() []string {
	return []string{"/DK", "/UYELER/DK"}
}

// PrivilegedReaderRootsFromEnv reads PrivilegedReaderRootsEnv against the
// Privileged roots: paths separated by commas, blanks around each ignored.
// Unset or blank is those of DefaultPrivilegedReaderRoots that are roots;
// "none" is no reader root (DK writes again, the way back). Each path set
// must be one of roots and named once, no ADMIN root may be one, and at
// least one root must stay a writer; anything else is an error that names
// the variable.
func PrivilegedReaderRootsFromEnv(getenv func(string) string, roots []string) ([]string, error) {
	raw := strings.TrimSpace(getenv(PrivilegedReaderRootsEnv))
	switch raw {
	case "":
		out := []string{}
		for _, root := range DefaultPrivilegedReaderRoots() {
			if slices.Contains(roots, root) {
				out = append(out, root)
			}
		}
		return out, nil
	case "none":
		return []string{}, nil
	}
	out := []string{}
	for _, entry := range strings.Split(raw, ",") {
		root := strings.TrimSpace(entry)
		switch {
		case root == "":
			return nil, fmt.Errorf("%s: an empty path in %q", PrivilegedReaderRootsEnv, raw)
		case !slices.Contains(roots, root):
			return nil, fmt.Errorf("%s: %q is not one of %s", PrivilegedReaderRootsEnv, root, PrivilegedRootsEnv)
		case slices.Contains(out, root):
			return nil, fmt.Errorf("%s: %q is named twice", PrivilegedReaderRootsEnv, root)
		case RootLabel(root) == "ADMIN":
			// ADMIN reading only would leave nobody to fix a mistake.
			return nil, fmt.Errorf("%s: %q is an ADMIN root and is never read-only", PrivilegedReaderRootsEnv, root)
		}
		out = append(out, root)
	}
	if len(out) == len(roots) {
		return nil, fmt.Errorf("%s: every root of %s would read only; nobody would change anything", PrivilegedReaderRootsEnv, PrivilegedRootsEnv)
	}
	return out, nil
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
