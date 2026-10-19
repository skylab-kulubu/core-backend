package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// modulePackage is a package of the module as `go list -test` shows it.
type modulePackage struct {
	ImportPath string
	// Dir is relative to the module root, "/"-separated ("." for the root).
	Dir string
	// TestDeps are the packages the package's test binary is built from, the
	// package itself included; nil when the package has no tests.
	TestDeps []string
	// ReadsRepo: its tests read files outside the package's directory (a
	// runbook under docs/, every migration), which no import shows.
	ReadsRepo bool
}

// readsRepo finds a test that reads files above its package: a ".." path
// element or a "../" prefix in a string literal.
var readsRepo = regexp.MustCompile(`"\.\."|"\.\./`)

// loadPackages lists the module's packages, their test binaries' imports and
// whether their tests read files above their directory.
func loadPackages(root string) ([]modulePackage, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command("go", "list", "-test", "-f", "{{.ImportPath}}\t{{.Dir}}\t{{join .Deps \" \"}}\t{{join .TestGoFiles \" \"}} {{join .XTestGoFiles \" \"}}", "./...")
	cmd.Dir = root
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list: %w", err)
	}
	byPath := map[string]*modulePackage{}
	testDeps := map[string][]string{}
	var order []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) != 4 {
			return nil, fmt.Errorf("go list: unexpected line %q", line)
		}
		importPath, dir, deps, testFiles := fields[0], fields[1], fields[2], strings.Fields(fields[3])
		switch {
		case strings.Contains(importPath, " "):
			// A package recompiled for a test ("p [p.test]"): its test
			// binary's entry lists everything it brings in.
			continue
		case strings.HasSuffix(importPath, ".test"):
			var list []string
			for _, dep := range strings.Fields(deps) {
				dep, _, _ = strings.Cut(dep, " ")
				list = append(list, dep)
			}
			testDeps[strings.TrimSuffix(importPath, ".test")] = list
			continue
		}
		rel, err := filepath.Rel(root, dir)
		if err != nil {
			return nil, err
		}
		p := &modulePackage{ImportPath: importPath, Dir: filepath.ToSlash(rel)}
		for _, file := range testFiles {
			raw, err := os.ReadFile(filepath.Join(dir, file))
			if err != nil {
				return nil, err
			}
			if readsRepo.Match(raw) {
				p.ReadsRepo = true
				break
			}
		}
		byPath[importPath] = p
		order = append(order, importPath)
	}
	var pkgs []modulePackage
	for _, importPath := range order {
		p := byPath[importPath]
		if deps, ok := testDeps[importPath]; ok {
			p.TestDeps = append([]string{importPath}, deps...)
		}
		pkgs = append(pkgs, *p)
	}
	return pkgs, nil
}

// affected lists the packages with tests that changed (repository-relative,
// "/"-separated paths) can affect:
//   - go.mod or go.sum: every package;
//   - a file in a package's directory, or below it (testdata, embedded
//     files): that package and each package above it, and every package
//     whose test binary imports one of them;
//   - Markdown outside every package (docs/): no package by itself;
//   - any other file outside every package (the workflow, the Dockerfiles,
//     deploy/): every package;
//
// and, whenever anything changed, the packages whose tests read files above
// their directory (ReadsRepo).
func affected(pkgs []modulePackage, changed []string) []string {
	byDir := map[string]string{}
	for _, p := range pkgs {
		byDir[p.Dir] = p.ImportPath
	}
	all := false
	direct := map[string]bool{}
	for _, file := range changed {
		file = strings.TrimPrefix(path.Clean(file), "./")
		if file == "go.mod" || file == "go.sum" {
			all = true
			break
		}
		found := false
		for dir := path.Dir(file); ; dir = path.Dir(dir) {
			if importPath, ok := byDir[dir]; ok {
				direct[importPath] = true
				found = true
			}
			if dir == "." {
				break
			}
		}
		if !found && !strings.HasSuffix(file, ".md") {
			all = true
			break
		}
	}
	var out []string
	for _, p := range pkgs {
		if p.TestDeps == nil {
			continue
		}
		hit := all || (len(changed) > 0 && p.ReadsRepo)
		for _, dep := range p.TestDeps {
			if hit {
				break
			}
			hit = direct[dep]
		}
		if hit {
			out = append(out, p.ImportPath)
		}
	}
	sort.Strings(out)
	return out
}

// affectedCommand prints, one per line, the packages whose tests the paths on
// stdin (one per line) can affect; with -all, every package with tests.
func affectedCommand(args []string) error {
	flags := flag.NewFlagSet("affected", flag.ContinueOnError)
	everything := flags.Bool("all", false, "every package with tests, whatever changed")
	if err := flags.Parse(args); err != nil {
		return err
	}
	pkgs, err := loadPackages(".")
	if err != nil {
		return err
	}
	var changed []string
	if *everything {
		changed = []string{"go.mod"}
	} else {
		changed, err = readLines(os.Stdin)
		if err != nil {
			return err
		}
	}
	for _, p := range affected(pkgs, changed) {
		fmt.Println(p)
	}
	return nil
}

func readLines(r io.Reader) ([]string, error) {
	var lines []string
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		if line := string(bytes.TrimSpace(scanner.Bytes())); line != "" {
			lines = append(lines, line)
		}
	}
	return lines, scanner.Err()
}
