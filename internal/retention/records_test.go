package retention_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// recordDeletion matches a statement that removes destruction records: a
// run, a rule's counts or a period.
var recordDeletion = regexp.MustCompile(`(?is)\b(DELETE\s+FROM|TRUNCATE(\s+TABLE)?|DROP\s+TABLE(\s+IF\s+EXISTS)?)\s+(ONLY\s+)?("?public"?\s*\.\s*)?"?retention_(runs|run_rules|periods)\b`)

// TestNoCodePathDeletesRetentionRecords keeps the record of every deletion,
// destruction and anonymisation the sweep made for at least three years
// (KVKK deletion regulation art. 7(3), ADR-0062). Today no code path deletes
// these rows at any age, so none can delete one younger than three years.
// A purge that is ever added must keep three years and change this guard
// knowingly.
//
// Down migrations are left out: the records' own refuses to run while any
// record exists.
func TestNoCodePathDeletesRetentionRecords(t *testing.T) {
	t.Parallel()

	for _, sample := range []string{
		"DELETE FROM retention_runs WHERE id = $1",
		"delete\n\t\tfrom public.retention_run_rules",
		`TRUNCATE TABLE "retention_periods"`,
		"DROP TABLE IF EXISTS retention_runs",
	} {
		if !recordDeletion.MatchString(sample) {
			t.Fatalf("guard does not recognise %q", sample)
		}
	}
	for _, sample := range []string{
		"SELECT rule FROM retention_run_rules",
		"UPDATE retention_runs SET status = 'abandoned'",
		"DELETE FROM url_hits",
	} {
		if recordDeletion.MatchString(sample) {
			t.Fatalf("guard matches the unrelated %q", sample)
		}
	}

	root := filepath.Join("..", "..")
	scanned := 0
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if name := entry.Name(); path != root && (strings.HasPrefix(name, ".") || name == "node_modules" || name == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		name := entry.Name()
		code := strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go")
		upMigration := strings.HasSuffix(name, ".up.sql")
		if !code && !upMigration {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		scanned++
		for _, match := range recordDeletion.FindAllIndex(raw, -1) {
			line := 1 + strings.Count(string(raw[:match[0]]), "\n")
			t.Errorf("%s:%d deletes retention records: %q", path, line, raw[match[0]:match[1]])
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned < 50 {
		t.Fatalf("scanned only %d files; the guard is not looking at the module", scanned)
	}
}
