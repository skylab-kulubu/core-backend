package certificate_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An Event copy shares its source's version asset copies
// (docs/certificate-event-templates.md#copy-and-team-change): that is safe
// only while no version is deleted and no asset copy is removed but a failed
// publish's own (discardPrivateCopies). A change that starts deleting either
// fails here and needs reference counting first.
func TestVersionsAndTheirAssetCopiesAreNeverDeleted(t *testing.T) {
	root := filepath.Join("..", "..")
	var found []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == ".git" || name == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, "_test.go") || strings.HasSuffix(path, ".down.sql") ||
			!(strings.HasSuffix(path, ".go") || strings.HasSuffix(path, ".sql")) {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		text := strings.ToLower(string(body))
		if strings.Contains(text, "delete from certificate_template_versions") ||
			strings.Contains(text, "delete from certificate_templates") {
			found = append(found, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) > 0 {
		t.Fatalf("template versions deleted in %v: version asset copies may be shared by an Event copy", found)
	}
}
