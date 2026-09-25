package account_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// proofDeletion matches a statement that removes erasure proof rows: the
// deletion request (which the step rows follow by ON DELETE CASCADE) or a step.
var proofDeletion = regexp.MustCompile(`(?is)\b(DELETE\s+FROM|TRUNCATE(\s+TABLE)?|DROP\s+TABLE(\s+IF\s+EXISTS)?)\s+(ONLY\s+)?("?public"?\s*\.\s*)?"?account_deletion_(requests|steps)\b`)

// TestNoCodePathDeletesErasureProof keeps the completion proof of an account
// erasure (the request with its dates, the step checkpoints and the service
// counts) for at least three years (KVKK deletion regulation art. 7(3); spec
// §5). Today no code path deletes these rows at any age, so none can delete a
// row younger than three years. A purge that is ever added must keep rows for
// three years and change this guard knowingly.
//
// Down migrations are left out: each one refuses to run while a deletion
// request exists.
func TestNoCodePathDeletesErasureProof(t *testing.T) {
	t.Parallel()

	for _, sample := range []string{
		"DELETE FROM account_deletion_requests WHERE id = $1",
		"delete\n\t\tfrom public.account_deletion_steps",
		`TRUNCATE TABLE "account_deletion_requests"`,
		"DROP TABLE IF EXISTS account_deletion_steps",
	} {
		if !proofDeletion.MatchString(sample) {
			t.Fatalf("guard does not recognise %q", sample)
		}
	}
	for _, sample := range []string{
		"DELETE FROM account_deletion_self_intakes",
		"DELETE FROM account_deletion_outbox",
		"SELECT step FROM account_deletion_steps",
	} {
		if proofDeletion.MatchString(sample) {
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
		for _, match := range proofDeletion.FindAllIndex(raw, -1) {
			line := 1 + strings.Count(string(raw[:match[0]]), "\n")
			t.Errorf("%s:%d deletes erasure proof: %q", path, line, raw[match[0]:match[1]])
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
