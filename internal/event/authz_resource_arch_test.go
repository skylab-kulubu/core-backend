package event_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// resourcesFromAnEvent are the places, by file, that still build an
// authz.Resource out of an Event's Owner team instead of reading
// event.AuthzResource (docs/co-organizers.md). Such a decision does not see
// the Event's Co-organizers. Co-organizer ticket 03 emptied it: it stays
// empty.
var resourcesFromAnEvent = map[string]int{}

// ownerTeamOnPurpose are the decisions that read an Owner team alone on
// purpose and stay: creating an Event in a team and moving an Event
// between Owner teams (the Owner team's, never a Co-organizer's); a
// certificate template's own team (a team template is its team's to read
// and write, whichever Events use it); and a certificate whose Event can
// no longer be read, decided by the Owner team it was issued under.
var ownerTeamOnPurpose = map[string]int{
	"internal/event/service.go":                4,
	"internal/certificate/event_template.go":   2,
	"internal/certificate/template_service.go": 4,
	"internal/certificate/service.go":          1,
}

// Every authz.Resource{..., OwnerTeam: x.OwnerTeam, ...}, and every call of
// a resource helper with a record's Owner team (resource(ev.OwnerTeam)),
// outside internal/authz and event.AuthzResource is one of
// resourcesFromAnEvent: a new one is a decision that misses the Event's
// Co-organizers. In internal/event the helper's calls are the Owner team's
// own decisions on purpose (a move between Owner teams).
func TestResourcesFromAnEventGoThroughAuthzResource(t *testing.T) {
	t.Parallel()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]int{}
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			if d.IsDir() {
				if rel == "internal/authz" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
				return nil
			}
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if err != nil {
				return err
			}
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if ok && rel == "internal/event/authz_resource.go" && fn.Name.Name == "AuthzResource" {
					continue
				}
				ast.Inspect(decl, func(n ast.Node) bool {
					switch n := n.(type) {
					case *ast.CompositeLit:
						if isAuthzResource(n.Type) && ownerTeamOfARecord(n) {
							found[rel]++
						}
					case *ast.CallExpr:
						if resourceHelper(n) {
							found[rel]++
						}
					}
					return true
				})
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	allowed := maps.Clone(resourcesFromAnEvent)
	for file, n := range ownerTeamOnPurpose {
		allowed[file] += n
	}
	for _, file := range slices.Sorted(maps.Keys(found)) {
		if found[file] > allowed[file] {
			t.Errorf("%q: %d, // builds authz.Resource out of a record's OwnerTeam (allowed %d): read event.AuthzResource", file, found[file], allowed[file])
		}
	}
	for _, file := range slices.Sorted(maps.Keys(allowed)) {
		if found[file] < allowed[file] {
			t.Errorf("%q: %d, // the list says %d: lower it", file, found[file], allowed[file])
		}
	}
}

func isAuthzResource(expr ast.Expr) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Resource" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "authz"
}

// ownerTeamOfARecord reports whether the literal sets OwnerTeam to some
// record's OwnerTeam field (ev.OwnerTeam, existing.OwnerTeam).
func ownerTeamOfARecord(lit *ast.CompositeLit) bool {
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok || key.Name != "OwnerTeam" {
			continue
		}
		if sel, ok := kv.Value.(*ast.SelectorExpr); ok && sel.Sel.Name == "OwnerTeam" {
			return true
		}
	}
	return false
}

// resourceHelper reports whether call is a resource helper (a function
// whose name ends in "resource" or "Resource") given a record's OwnerTeam.
func resourceHelper(call *ast.CallExpr) bool {
	var name string
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		name = fn.Name
	case *ast.SelectorExpr:
		name = fn.Sel.Name
	}
	if !strings.HasSuffix(strings.ToLower(name), "resource") || name == "AuthzResource" {
		return false
	}
	for _, arg := range call.Args {
		if sel, ok := arg.(*ast.SelectorExpr); ok && sel.Sel.Name == "OwnerTeam" {
			return true
		}
	}
	return false
}
