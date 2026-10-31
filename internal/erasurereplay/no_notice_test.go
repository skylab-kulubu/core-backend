package erasurereplay

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// A replay after a restore sends the services their Erasure commands again
// and nothing else: the person was told when their request was received and
// when it completed (docs/account-lifecycle.md, "Deletion notices"), and is
// never told twice. Neither the replay nor its command reaches a mailer or
// the erasure worker that sends the notices.
func TestReplaySendsNoDeletionNotice(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, filepath.Join("..", "..", "cmd", "core-backend", "replay_from_backup.go"))
	forbiddenImports := map[string]bool{"github.com/skylab-kulubu/core-backend/internal/mail": true}
	forbiddenNames := map[string]bool{"DeletionNotices": true, "AccountDeletionNotice": true, "NewWorker": true, "Maintain": true, "RunOnce": true}
	checked := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		checked++
		for _, spec := range file.Imports {
			path, _ := strconv.Unquote(spec.Path.Value)
			if forbiddenImports[path] {
				t.Errorf("%s imports %s", name, path)
			}
		}
		ast.Inspect(file, func(node ast.Node) bool {
			if selector, ok := node.(*ast.SelectorExpr); ok && forbiddenNames[selector.Sel.Name] {
				t.Errorf("%s uses %s", name, selector.Sel.Name)
			}
			return true
		})
	}
	if checked < 3 {
		t.Fatalf("checked %d files, want the package and the command", checked)
	}
}
