package main

import (
	"slices"
	"testing"
)

// A small module: api imports store, store imports nothing of ours; docsreader
// reads docs/ from its tests; tool has no tests.
var modulePackages = []modulePackage{
	{ImportPath: "m/internal/store", Dir: "internal/store", TestDeps: []string{"m/internal/store"}},
	{ImportPath: "m/internal/api", Dir: "internal/api", TestDeps: []string{"m/internal/api", "m/internal/store"}},
	{ImportPath: "m/internal/api/apitest", Dir: "internal/api/apitest", TestDeps: []string{"m/internal/api/apitest"}},
	{ImportPath: "m/internal/docsreader", Dir: "internal/docsreader", TestDeps: []string{"m/internal/docsreader"}, ReadsRepo: true},
	{ImportPath: "m/cmd/tool", Dir: "cmd/tool"},
}

func TestAffectedFollowsTheTestBinariesImports(t *testing.T) {
	for _, tc := range []struct {
		name    string
		changed []string
		want    []string
	}{
		{"a package's code", []string{"internal/store/store.go"},
			[]string{"m/internal/api", "m/internal/docsreader", "m/internal/store"}},
		{"a leaf's test", []string{"internal/api/api_test.go"},
			[]string{"m/internal/api", "m/internal/docsreader"}},
		{"a package's embedded or testdata file, and the packages above it", []string{"internal/api/apitest/testdata/x.json"},
			[]string{"m/internal/api", "m/internal/api/apitest", "m/internal/docsreader"}},
		{"documentation: only the tests that read it", []string{"docs/runbook.md", "AGENTS.md"},
			[]string{"m/internal/docsreader"}},
		{"go.mod", []string{"go.mod"},
			[]string{"m/internal/api", "m/internal/api/apitest", "m/internal/docsreader", "m/internal/store"}},
		{"a file outside every package", []string{"internal/store/store.go", "Dockerfile"},
			[]string{"m/internal/api", "m/internal/api/apitest", "m/internal/docsreader", "m/internal/store"}},
		{"the workflow", []string{".github/workflows/checks.yml"},
			[]string{"m/internal/api", "m/internal/api/apitest", "m/internal/docsreader", "m/internal/store"}},
		{"a package without tests", []string{"cmd/tool/main.go"},
			[]string{"m/internal/docsreader"}},
		{"nothing", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := affected(modulePackages, tc.changed); !slices.Equal(got, tc.want) {
				t.Fatalf("affected(%v) = %v, want %v", tc.changed, got, tc.want)
			}
		})
	}
}

// The real module: go list finds the packages, and a test reading docs/ is
// one that reads the repository.
func TestLoadPackagesReadsTheModule(t *testing.T) {
	pkgs, err := loadPackages("../../..")
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]modulePackage{}
	for _, p := range pkgs {
		byPath[p.ImportPath] = p
	}
	const module = "github.com/skylab-kulubu/core-backend/"
	media, ok := byPath[module+"internal/media"]
	if !ok || media.Dir != "internal/media" || !slices.Contains(media.TestDeps, module+"internal/migrate") || !slices.Contains(media.TestDeps, module+"internal/media") {
		t.Fatalf("internal/media = %+v", media)
	}
	if account := byPath[module+"internal/account"]; !account.ReadsRepo {
		t.Fatal("internal/account's tests read docs/account-lifecycle.md, not seen")
	}
	if db, ok := byPath[module+"db"]; !ok || db.TestDeps != nil {
		t.Fatalf("db (no tests) = %+v", db)
	}
}
