package main

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"testing"
)

func TestTestNamesFollowGoTestsRules(t *testing.T) {
	dir := t.TempDir()
	write := func(name, src string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a_test.go", `package a
import "testing"
func TestMain(m *testing.M) {}
func TestA(t *testing.T) {}
func Test(t *testing.T) {}
func Testlower(t *testing.T) {}
func Test_underscore(t *testing.T) {}
func FuzzB(f *testing.F) {}
func ExampleC() {}
func helper(t *testing.T) {}
type s struct{}
func (s) TestMethod(t *testing.T) {}
`)
	write("b_test.go", `package a_test
import "testing"
func TestA(t *testing.T) {}
func TestÜmlaut(t *testing.T) {}
`)
	write("a.go", `package a
func TestNotATestFile() {}
`)
	names, err := testNames(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"ExampleC", "FuzzB", "Test", "TestA", "Test_underscore", "TestÜmlaut"}
	if !slices.Equal(names, want) {
		t.Fatalf("names %v, want %v", names, want)
	}
}

func TestExpectedUsesTimingsAndTheMedianForNewTests(t *testing.T) {
	p := expected("p", []string{"A", "B", "C", "New", "Zero"}, Timings{"p": {"A": 10, "B": 2, "C": 4, "Zero": 0, "Gone": 99}})
	want := map[string]float64{"A": 10, "B": 2, "C": 4, "New": 4, "Zero": minTestSeconds}
	for name, seconds := range want {
		if p.Seconds[name] != seconds {
			t.Errorf("%s: %v, want %v", name, p.Seconds[name], seconds)
		}
	}
	if q := expected("q", []string{"X"}, nil); q.Seconds["X"] != 1 {
		t.Errorf("without timings: %v, want 1", q.Seconds["X"])
	}
}

// Every test runs exactly once over a plan's units: in one part's -run, or in
// the last part, which skips what the others run.
func TestPlanRunsEveryTestOnce(t *testing.T) {
	var pkgs []packageTests
	for _, spec := range []struct {
		pkg   string
		tests int
		each  float64
	}{{"big", 400, 1.8}, {"mid", 80, 5}, {"small", 10, 0.1}, {"one", 1, 300}, {"none", 0, 0}} {
		p := packageTests{Package: spec.pkg, Seconds: map[string]float64{}}
		for i := range spec.tests {
			name := "Test" + spec.pkg + string(rune('A'+i%26)) + string(rune('a'+i/26))
			p.Names = append(p.Names, name)
			p.Seconds[name] = spec.each * float64(1+i%3)
		}
		sort.Strings(p.Names)
		pkgs = append(pkgs, p)
	}
	plan := makePlan(pkgs, planOptions{Target: 300, MaxShards: 6, Parallel: 4})
	if len(plan.Shards) != 6 {
		t.Fatalf("%d shards, want 6", len(plan.Shards))
	}
	ran := map[string]int{}
	for _, shard := range plan.Shards {
		for _, u := range shard {
			for _, p := range pkgs {
				if p.Package != u.Package {
					continue
				}
				for _, name := range p.Names {
					if runs(u, name) {
						ran[u.Package+"."+name]++
					}
				}
			}
		}
	}
	for _, p := range pkgs {
		for _, name := range p.Names {
			if n := ran[p.Package+"."+name]; n != 1 {
				t.Errorf("%s.%s runs %d times", p.Package, name, n)
			}
		}
	}
	// A test the plan did not see runs in the last part of its package.
	for _, shard := range plan.Shards {
		for _, u := range shard {
			if u.Package == "big" && u.Part == u.Parts && !runs(u, "TestNew") {
				t.Errorf("%s does not run a new test", u.Name())
			}
			if u.Package == "big" && u.Part != u.Parts && runs(u, "TestNew") {
				t.Errorf("%s runs a test it does not list", u.Name())
			}
		}
	}
}

// runs says whether `go test` would run the top-level test name in u.
func runs(u Unit, name string) bool {
	args := u.Args()
	for i := 0; i+1 < len(args); i += 2 {
		matched := regexp.MustCompile(args[i+1]).MatchString(name)
		if (args[i] == "-run" && !matched) || (args[i] == "-skip" && matched) {
			return false
		}
	}
	return true
}

func TestPlanBalancesTheShards(t *testing.T) {
	var pkgs []packageTests
	for i := range 40 {
		p := packageTests{Package: "p" + string(rune('a'+i%26)) + string(rune('a'+i/26)), Seconds: map[string]float64{}}
		for j := range 20 {
			name := "Test" + string(rune('A'+j))
			p.Names = append(p.Names, name)
			p.Seconds[name] = float64(1 + (i*j)%7)
		}
		pkgs = append(pkgs, p)
	}
	plan := makePlan(pkgs, planOptions{Target: 600, MaxShards: 8, Parallel: 4})
	lightest, heaviest := 1e9, 0.0
	for _, shard := range plan.Shards {
		load := 0.0
		for _, u := range shard {
			load += u.Weight
		}
		lightest, heaviest = min(lightest, load), max(heaviest, load)
	}
	if heaviest > lightest*1.15 {
		t.Fatalf("shards from %.0fs to %.0fs", lightest, heaviest)
	}
}

func TestASmallPlanIsOneShardAndAPackageWithOneTestIsNotSplit(t *testing.T) {
	plan := makePlan([]packageTests{
		{Package: "a", Names: []string{"TestA", "TestB"}, Seconds: map[string]float64{"TestA": 1, "TestB": 1}},
		{Package: "long", Names: []string{"TestLong"}, Seconds: map[string]float64{"TestLong": 500}},
	}, planOptions{Target: 600, MaxShards: 8, Parallel: 4})
	if len(plan.Shards) != 1 || len(plan.Shards[0]) != 2 {
		t.Fatalf("plan %+v", plan)
	}
	for _, u := range plan.Shards[0] {
		if u.Parts != 0 || len(u.Run) != 0 || len(u.Skip) != 0 {
			t.Fatalf("unit %+v split", u)
		}
	}
}
