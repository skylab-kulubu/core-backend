package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Plan is the tests of a CI run, split over shards (jobs).
type Plan struct {
	Shards [][]Unit `json:"shards"`
}

// Unit is one `go test` of one package: all of its tests, or a part of them.
type Unit struct {
	Package string `json:"package"`
	// Part and Parts number a long package's parts (1-based); 0 when the
	// unit is the whole package.
	Part  int `json:"part,omitempty"`
	Parts int `json:"parts,omitempty"`
	// Run lists the part's tests; Skip, in the package's last part, the
	// tests of its other parts. That part runs every other test, also one
	// the plan did not see.
	Run  []string `json:"run,omitempty"`
	Skip []string `json:"skip,omitempty"`
	// Weight is the expected seconds of test time.
	Weight float64 `json:"weight"`
}

// Name is how the logs name the unit.
func (u Unit) Name() string {
	if u.Parts == 0 {
		return u.Package
	}
	return fmt.Sprintf("%s (part %d/%d)", u.Package, u.Part, u.Parts)
}

// Args are the unit's `go test` arguments after the shared flags.
func (u Unit) Args() []string {
	var args []string
	if len(u.Run) > 0 {
		args = append(args, "-run", namesPattern(u.Run))
	}
	if len(u.Skip) > 0 {
		args = append(args, "-skip", namesPattern(u.Skip))
	}
	return append(args, u.Package)
}

// namesPattern matches exactly the named top-level tests (and so their
// subtests).
func namesPattern(names []string) string {
	quoted := make([]string, len(names))
	for i, name := range names {
		quoted[i] = regexp.QuoteMeta(name)
	}
	return "^(" + strings.Join(quoted, "|") + ")$"
}

// Timings are seconds by package and top-level test.
type Timings map[string]map[string]float64

// readTimings reads "package<TAB>test<TAB>seconds" lines; a missing file is
// no timings.
func readTimings(file string) (Timings, error) {
	timings := Timings{}
	if file == "" {
		return timings, nil
	}
	f, err := os.Open(file)
	if errors.Is(err, fs.ErrNotExist) {
		return timings, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Split(scanner.Text(), "\t")
		if len(fields) != 3 {
			continue
		}
		seconds, err := strconv.ParseFloat(fields[2], 64)
		if err != nil || seconds < 0 {
			continue
		}
		timings.set(fields[0], fields[1], seconds)
	}
	return timings, scanner.Err()
}

func (t Timings) set(pkg, test string, seconds float64) {
	if t[pkg] == nil {
		t[pkg] = map[string]float64{}
	}
	t[pkg][test] = seconds
}

func (t Timings) write(file string) error {
	var lines []string
	for pkg, tests := range t {
		for test, seconds := range tests {
			lines = append(lines, fmt.Sprintf("%s\t%s\t%.2f", pkg, test, seconds))
		}
	}
	sort.Strings(lines)
	return os.WriteFile(file, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
}

// testNames are the top-level tests, fuzz tests and examples `go test` runs
// in the package in dir, by its _test.go files.
func testNames(dir string) ([]string, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*_test.go"))
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	fset := token.NewFileSet()
	for _, file := range files {
		parsed, err := parser.ParseFile(fset, file, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil {
				continue
			}
			name := fn.Name.Name
			if name != "TestMain" && (isTest(name, "Test") || isTest(name, "Fuzz") || isTest(name, "Example")) {
				seen[name] = true
			}
		}
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// isTest is go test's own rule: the prefix, then nothing or no lower-case
// letter.
func isTest(name, prefix string) bool {
	if !strings.HasPrefix(name, prefix) {
		return false
	}
	if len(name) == len(prefix) {
		return true
	}
	r, _ := utf8.DecodeRuneInString(name[len(prefix):])
	return !unicode.IsLower(r)
}

// packageTests are a package's tests with their expected seconds.
type packageTests struct {
	Package string
	Names   []string
	Seconds map[string]float64
}

func (p packageTests) weight() float64 {
	total := 0.0
	for _, name := range p.Names {
		total += p.Seconds[name]
	}
	return total
}

// minTestSeconds is the least a test is expected to take.
const minTestSeconds = 0.01

// unitOverhead is a unit's own cost (building and starting the test binary),
// in seconds.
const unitOverhead = 3

// expected gives each test the seconds it last took; a test without a timing
// (new, or no timings yet), the median of its package's known tests, or a
// second.
func expected(pkg string, names []string, timings Timings) packageTests {
	known := timings[pkg]
	var samples []float64
	for _, name := range names {
		if seconds, ok := known[name]; ok {
			samples = append(samples, seconds)
		}
	}
	fallback := 1.0
	if len(samples) > 0 {
		sort.Float64s(samples)
		fallback = samples[len(samples)/2]
	}
	seconds := map[string]float64{}
	for _, name := range names {
		s, ok := known[name]
		if !ok {
			s = fallback
		}
		// Every test weighs something, so each part of a split gets one.
		seconds[name] = max(s, minTestSeconds)
	}
	return packageTests{Package: pkg, Names: names, Seconds: seconds}
}

// planOptions shape a plan: shards of about Target seconds of test time, at
// most MaxShards of them; a shard runs Parallel units at once, so no unit
// should take longer than a shard's share for one of them.
type planOptions struct {
	Target    float64
	MaxShards int
	Parallel  int
}

// makePlan splits the packages' tests over shards.
func makePlan(pkgs []packageTests, opts planOptions) Plan {
	total := 0.0
	for _, p := range pkgs {
		total += p.weight() + unitOverhead
	}
	shards := int(math.Ceil(total / opts.Target))
	shards = max(1, min(shards, opts.MaxShards))
	unitCap := max(30, total/float64(shards)/float64(max(1, opts.Parallel)))

	var units []Unit
	for _, p := range pkgs {
		units = append(units, splitPackage(p, unitCap)...)
	}
	sort.SliceStable(units, func(i, j int) bool {
		if units[i].Weight != units[j].Weight {
			return units[i].Weight > units[j].Weight
		}
		return units[i].Name() < units[j].Name()
	})
	plan := Plan{Shards: make([][]Unit, shards)}
	loads := make([]float64, shards)
	for _, u := range units {
		lightest := 0
		for i := range loads {
			if loads[i] < loads[lightest] {
				lightest = i
			}
		}
		plan.Shards[lightest] = append(plan.Shards[lightest], u)
		loads[lightest] += u.Weight
	}
	kept := plan.Shards[:0]
	for _, shard := range plan.Shards {
		if len(shard) > 0 {
			kept = append(kept, shard)
		}
	}
	plan.Shards = kept
	return plan
}

// splitPackage keeps a package whole, or splits a package longer than
// unitCap into parts of about unitCap, heaviest test first into the lightest
// part. The part with the most tests becomes the last: it runs every test the
// other parts do not.
func splitPackage(p packageTests, unitCap float64) []Unit {
	weight := p.weight()
	parts := int(math.Ceil(weight / unitCap))
	if parts > len(p.Names) {
		parts = len(p.Names)
	}
	if parts <= 1 {
		return []Unit{{Package: p.Package, Weight: weight + unitOverhead}}
	}
	names := append([]string(nil), p.Names...)
	sort.SliceStable(names, func(i, j int) bool {
		if p.Seconds[names[i]] != p.Seconds[names[j]] {
			return p.Seconds[names[i]] > p.Seconds[names[j]]
		}
		return names[i] < names[j]
	})
	lists := make([][]string, parts)
	loads := make([]float64, parts)
	for _, name := range names {
		lightest := 0
		for i := range loads {
			if loads[i] < loads[lightest] {
				lightest = i
			}
		}
		lists[lightest] = append(lists[lightest], name)
		loads[lightest] += p.Seconds[name]
	}
	// A part without tests would run the whole package (no -run).
	for i := 0; i < len(lists); {
		if len(lists[i]) == 0 {
			lists = append(lists[:i], lists[i+1:]...)
			loads = append(loads[:i], loads[i+1:]...)
			continue
		}
		i++
	}
	parts = len(lists)
	if parts == 1 {
		return []Unit{{Package: p.Package, Weight: weight + unitOverhead}}
	}
	rest := 0
	for i := range lists {
		if len(lists[i]) > len(lists[rest]) {
			rest = i
		}
	}
	lists[rest], lists[parts-1] = lists[parts-1], lists[rest]
	loads[rest], loads[parts-1] = loads[parts-1], loads[rest]

	var others []string
	units := make([]Unit, parts)
	for i := range parts - 1 {
		run := append([]string(nil), lists[i]...)
		sort.Strings(run)
		others = append(others, run...)
		units[i] = Unit{Package: p.Package, Part: i + 1, Parts: parts, Run: run, Weight: loads[i] + unitOverhead}
	}
	sort.Strings(others)
	units[parts-1] = Unit{Package: p.Package, Part: parts, Parts: parts, Skip: others, Weight: loads[parts-1] + unitOverhead}
	return units
}

// planCommand writes the plan of the listed packages and prints its shards
// as a GitHub Actions matrix ({"shard":[0,1,…]}).
func planCommand(args []string) error {
	flags := flag.NewFlagSet("plan", flag.ContinueOnError)
	packagesFile := flags.String("packages", "", "file listing the packages to test, one per line")
	timingsFile := flags.String("timings", "", "timings of earlier runs (missing: none)")
	out := flags.String("out", "testplan.json", "where to write the plan")
	target := flags.Float64("target", 420, "seconds of test time per shard")
	maxShards := flags.Int("max-shards", 8, "most shards")
	parallel := flags.Int("parallel", 4, "units a shard runs at once")
	if err := flags.Parse(args); err != nil {
		return err
	}
	f, err := os.Open(*packagesFile)
	if err != nil {
		return err
	}
	listed, err := readLines(f)
	f.Close()
	if err != nil {
		return err
	}
	timings, err := readTimings(*timingsFile)
	if err != nil {
		return err
	}
	dirs, err := packageDirs(listed)
	if err != nil {
		return err
	}
	var pkgs []packageTests
	for _, pkg := range listed {
		names, err := testNames(dirs[pkg])
		if err != nil {
			return err
		}
		pkgs = append(pkgs, expected(pkg, names, timings))
	}
	plan := makePlan(pkgs, planOptions{Target: *target, MaxShards: *maxShards, Parallel: *parallel})
	raw, err := json.Marshal(plan)
	if err != nil {
		return err
	}
	if err := os.WriteFile(*out, raw, 0o644); err != nil {
		return err
	}
	shards := make([]int, len(plan.Shards))
	for i, shard := range plan.Shards {
		shards[i] = i
		load := 0.0
		for _, u := range shard {
			load += u.Weight
		}
		fmt.Fprintf(os.Stderr, "shard %d: %d units, ~%.0fs of tests\n", i, len(shard), load)
		for _, u := range shard {
			fmt.Fprintf(os.Stderr, "  %-60s ~%.0fs\n", u.Name(), u.Weight)
		}
	}
	matrix, err := json.Marshal(map[string][]int{"shard": shards})
	if err != nil {
		return err
	}
	fmt.Println(string(matrix))
	return nil
}

// packageDirs finds the listed packages' directories.
func packageDirs(pkgs []string) (map[string]string, error) {
	dirs := map[string]string{}
	if len(pkgs) == 0 {
		return dirs, nil
	}
	cmd := exec.Command("go", append([]string{"list", "-f", "{{.ImportPath}}\t{{.Dir}}"}, pkgs...)...)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list: %w", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if importPath, dir, ok := strings.Cut(line, "\t"); ok {
			dirs[importPath] = dir
		}
	}
	return dirs, nil
}
