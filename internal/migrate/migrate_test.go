package migrate

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/skylab-kulubu/core-backend/db"
)

func TestVersionsIncludeExtraFormURLsInOrder(t *testing.T) {
	t.Parallel()
	got, err := Versions()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("no migrations")
	}
	found := false
	for i := 1; i < len(got); i++ {
		if got[i] <= got[i-1] {
			t.Fatalf("not increasing %v", got)
		}
	}
	for _, v := range got {
		if v == 20260918180000 {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("missing extra_form_urls version in %v", got)
	}
}

func TestExtraFormSQLIsIdempotent(t *testing.T) {
	t.Parallel()
	sql, err := ExtraFormSQL()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sql, "ADD COLUMN IF NOT EXISTS extra_form_urls") {
		t.Fatalf("sql %s", sql)
	}
}

func TestListStopsOnARepeatedVersion(t *testing.T) {
	t.Parallel()
	fsys := fstest.MapFS{
		"migrations/20261027120000_subject_requests.up.sql":   {Data: []byte("SELECT 1")},
		"migrations/20261027120000_contact_consents.up.sql":   {Data: []byte("SELECT 1")},
		"migrations/20261027110000_earlier.up.sql":            {Data: []byte("SELECT 1")},
		"migrations/20261027120000_subject_requests.down.sql": {Data: []byte("SELECT 1")},
	}
	_, err := list(fsys, ".up.sql")
	if err == nil {
		t.Fatal("two .up.sql files with version 20261027120000: want an error")
	}
	for _, want := range []string{"20261027120000", "20261027120000_contact_consents.up.sql", "20261027120000_subject_requests.up.sql"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not name %s", err, want)
		}
	}
	got, err := list(fsys, ".down.sql")
	if err != nil || len(got) != 1 {
		t.Fatalf("one .down.sql: got %v, %v", got, err)
	}
}

// Every version is used once, by one .up.sql and at most one .down.sql: two
// branches that picked the same number would otherwise leave core unable to
// start (the second file fails the first one's fingerprint) or skip one of
// the files silently on a database that has the version recorded.
func TestEveryVersionOnce(t *testing.T) {
	t.Parallel()
	up, err := list(db.UpSQL, ".up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if len(up) == 0 {
		t.Fatal("no migrations")
	}
	down, err := list(db.DownSQL, ".down.sql")
	if err != nil {
		t.Fatal(err)
	}
	ups := map[int64]bool{}
	for _, f := range up {
		ups[f.version] = true
	}
	for _, f := range down {
		if !ups[f.version] {
			t.Errorf("%s has no .up.sql of its version", f.name)
		}
	}
}

// withoutFingerprint are the migrations that have no entry in fingerprints:
// each is safe to run again on a schema that already has it (IF NOT EXISTS
// and the like), so it simply runs once more on a database whose
// schema_migrations lost it. A new migration gets a fingerprint, or is added
// here on purpose.
var withoutFingerprint = map[int64]bool{
	20260919010000: true,
	20260919140000: true,
	20260919150000: true,
	20261003230000: true,
	20261012170000: true,
	20261021153000: true,
	20261023120000: true,
	20261023180000: true,
	20261024190000: true,
	20261025120000: true,
	20261027130000: true,
	20261031200000: true,
}

func TestFingerprintsMatchTheMigrations(t *testing.T) {
	t.Parallel()
	files, err := listUp()
	if err != nil {
		t.Fatal(err)
	}
	versions := map[int64]bool{}
	for _, f := range files {
		versions[f.version] = true
		_, has := fingerprints[f.version]
		if has && withoutFingerprint[f.version] {
			t.Errorf("%s has a fingerprint: take %d out of withoutFingerprint", f.name, f.version)
		}
		if !has && !withoutFingerprint[f.version] {
			t.Errorf("%s has no fingerprint: add one to fingerprints in migrate.go", f.name)
		}
	}
	// A key without its file is a migration that was renumbered while its
	// fingerprint kept the old number: the fingerprint is then never used.
	for v := range fingerprints {
		if !versions[v] {
			t.Errorf("fingerprints has %d, but there is no migration of that version", v)
		}
	}
	for v := range withoutFingerprint {
		if !versions[v] {
			t.Errorf("withoutFingerprint has %d, but there is no migration of that version", v)
		}
	}
}
