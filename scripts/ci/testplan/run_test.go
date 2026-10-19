package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestReadResultFindsFailuresSkipsAndTimings(t *testing.T) {
	events := `{"Action":"start","Package":"p"}
{"Action":"run","Package":"p","Test":"TestOK"}
{"Action":"output","Package":"p","Test":"TestOK","Output":"=== RUN   TestOK\n"}
{"Action":"pass","Package":"p","Test":"TestOK","Elapsed":1.5}
{"Action":"run","Package":"p","Test":"TestBad"}
{"Action":"output","Package":"p","Test":"TestBad/sub","Output":"    bad_test.go:9: boom\n"}
{"Action":"fail","Package":"p","Test":"TestBad/sub","Elapsed":0.1}
{"Action":"fail","Package":"p","Test":"TestBad","Elapsed":0.2}
{"Action":"skip","Package":"p","Test":"TestDocker","Elapsed":0}
{"Action":"output","Package":"p","Output":"FAIL\n"}
{"Action":"fail","Package":"p","Elapsed":2}
`
	result, err := readResult(strings.NewReader(events))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(result.Failed, []string{"TestBad"}) || !slices.Equal(result.Skipped, []string{"TestDocker"}) {
		t.Fatalf("failed %v skipped %v", result.Failed, result.Skipped)
	}
	if result.Timings["TestOK"] != 1.5 || result.Timings["TestBad"] != 0.2 || len(result.Timings) != 2 {
		t.Fatalf("timings %v", result.Timings)
	}
	if !strings.Contains(result.Output["TestBad"].String(), "boom") || !strings.Contains(result.Output[""].String(), "FAIL") {
		t.Fatalf("output %v", result.Output)
	}
}

// A unit whose listed tests do not run (a build tag leaves them out) fails;
// one that runs its test passes.
func TestRunUnitFailsWhenNoTestRan(t *testing.T) {
	dir := t.TempDir()
	for name, src := range map[string]string{
		"go.mod":         "module example.com/m\n\ngo 1.25\n",
		"m.go":           "package m\n",
		"m_test.go":      "package m\n\nimport \"testing\"\n\nfunc TestRuns(t *testing.T) {}\n",
		"tagged_test.go": "//go:build never\n\npackage m\n\nimport \"testing\"\n\nfunc TestTagged(t *testing.T) {}\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(dir)
	out := t.TempDir()

	report, failed, _, _, err := runUnit(0, Unit{Package: "example.com/m", Part: 1, Parts: 2, Run: []string{"TestTagged"}}, []string{"-count=1"}, out)
	if err != nil || !failed || !strings.Contains(report, "no test ran") {
		t.Fatalf("tagged unit: failed %v err %v report %q", failed, err, report)
	}
	report, failed, _, timings, err := runUnit(1, Unit{Package: "example.com/m"}, []string{"-count=1"}, out)
	if err != nil || failed || len(timings) != 1 {
		t.Fatalf("whole unit: failed %v err %v timings %v report %q", failed, err, timings, report)
	}
}
