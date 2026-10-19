package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// testEvent is one line of `go test -json` (cmd/test2json).
type testEvent struct {
	Action  string
	Package string
	Test    string
	Elapsed float64
	Output  string
}

// unitResult is what one unit's `go test -json` output says.
type unitResult struct {
	// Failed are the failed top-level tests; Skipped every skipped test.
	Failed  []string
	Skipped []string
	// Timings are the seconds of each finished top-level test.
	Timings map[string]float64
	// Output is the output of each top-level test (subtests included), and
	// under "" the package's own.
	Output map[string]*strings.Builder
}

// readResult reads a unit's `go test -json` output.
func readResult(r io.Reader) (unitResult, error) {
	result := unitResult{Timings: map[string]float64{}, Output: map[string]*strings.Builder{}}
	failed := map[string]bool{}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		var event testEvent
		if len(line) == 0 || line[0] != '{' || json.Unmarshal(line, &event) != nil {
			// Not an event (go's own output): the package's.
			appendOutput(result.Output, "", string(line)+"\n")
			continue
		}
		top, _, _ := strings.Cut(event.Test, "/")
		switch event.Action {
		case "output", "build-output":
			appendOutput(result.Output, top, event.Output)
		case "skip":
			if event.Test != "" {
				result.Skipped = append(result.Skipped, event.Test)
			}
		case "pass", "fail":
			if event.Test != "" && event.Test == top {
				result.Timings[top] = event.Elapsed
			}
			if event.Action == "fail" && event.Test != "" && !failed[top] {
				failed[top] = true
				result.Failed = append(result.Failed, top)
			}
		}
	}
	return result, scanner.Err()
}

func appendOutput(outputs map[string]*strings.Builder, key, text string) {
	if outputs[key] == nil {
		outputs[key] = &strings.Builder{}
	}
	outputs[key].WriteString(text)
}

// runCommand runs one shard of a plan: each unit as `go test FLAGS -json`,
// Parallel at once, heaviest first. It prints a line per unit and the output
// of whatever failed, writes the tests' timings to DIR/timings.tsv, and fails
// when a unit failed or a test skipped itself (a test that cannot reach
// Docker or ffmpeg skips; none skips on purpose).
func runCommand(args []string) error {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	planFile := flags.String("plan", "testplan.json", "the plan")
	shard := flags.Int("shard", 0, "the shard to run")
	dir := flags.String("dir", "test-results", "where the units' output goes")
	parallel := flags.Int("parallel", runtime.NumCPU(), "units at once")
	if err := flags.Parse(args); err != nil {
		return err
	}
	goFlags := flags.Args()
	raw, err := os.ReadFile(*planFile)
	if err != nil {
		return err
	}
	var plan Plan
	if err := json.Unmarshal(raw, &plan); err != nil {
		return err
	}
	if *shard < 0 || *shard >= len(plan.Shards) {
		return fmt.Errorf("shard %d of %d", *shard, len(plan.Shards))
	}
	units := plan.Shards[*shard]
	if err := os.MkdirAll(*dir, 0o755); err != nil {
		return err
	}
	fmt.Printf("shard %d: %d units, %d at once; go test %s\n", *shard, len(units), *parallel, strings.Join(goFlags, " "))

	var (
		mu       sync.Mutex
		failed   []string
		skipped  []string
		timings  = Timings{}
		wg       sync.WaitGroup
		slots    = make(chan struct{}, max(1, *parallel))
		started  = time.Now()
		problems []error
	)
	for i, u := range units {
		wg.Add(1)
		go func() {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			report, unitFailed, unitSkipped, unitTimings, err := runUnit(i, u, goFlags, *dir)
			mu.Lock()
			defer mu.Unlock()
			fmt.Print(report)
			if err != nil {
				problems = append(problems, err)
			}
			if unitFailed {
				failed = append(failed, u.Name())
			}
			skipped = append(skipped, unitSkipped...)
			for test, seconds := range unitTimings {
				timings.set(u.Package, test, seconds)
			}
		}()
	}
	wg.Wait()
	if err := timings.write(filepath.Join(*dir, "timings.tsv")); err != nil {
		return err
	}
	fmt.Printf("shard %d finished in %s\n", *shard, time.Since(started).Round(time.Second))
	sort.Strings(failed)
	sort.Strings(skipped)
	if len(skipped) > 0 {
		fmt.Printf("\nskipped (often for want of Docker or ffmpeg; no test skips on purpose):\n  %s\n", strings.Join(skipped, "\n  "))
	}
	if len(failed) > 0 {
		fmt.Printf("\nfailed:\n  %s\n", strings.Join(failed, "\n  "))
	}
	if err := errors.Join(problems...); err != nil {
		return err
	}
	if len(failed) > 0 || len(skipped) > 0 {
		return fmt.Errorf("%d units failed, %d tests skipped", len(failed), len(skipped))
	}
	return nil
}

// runUnit runs one unit and returns what to print, whether it failed, the
// tests that skipped and the timings.
func runUnit(i int, u Unit, goFlags []string, dir string) (string, bool, []string, map[string]float64, error) {
	jsonFile := filepath.Join(dir, fmt.Sprintf("unit-%02d.json", i))
	out, err := os.Create(jsonFile)
	if err != nil {
		return "", true, nil, nil, err
	}
	defer out.Close()
	var stderr bytes.Buffer
	cmd := exec.Command("go", append(append(append([]string{"test"}, goFlags...), "-json"), u.Args()...)...)
	cmd.Stdout, cmd.Stderr = out, &stderr
	started := time.Now()
	runErr := cmd.Run()
	took := time.Since(started).Round(100 * time.Millisecond)
	if _, err := out.Seek(0, io.SeekStart); err != nil {
		return "", true, nil, nil, err
	}
	result, err := readResult(out)
	if err != nil {
		return "", true, nil, nil, err
	}
	var report strings.Builder
	var skipped []string
	for _, test := range result.Skipped {
		skipped = append(skipped, u.Package+" "+test)
	}
	unitFailed := runErr != nil || len(result.Failed) > 0
	status := "ok  "
	if unitFailed {
		status = "FAIL"
	}
	fmt.Fprintf(&report, "%s %s %s (%d tests)\n", status, u.Name(), took, len(result.Timings))
	if unitFailed {
		for _, test := range result.Failed {
			if output := result.Output[test]; output != nil {
				report.WriteString(output.String())
			}
		}
		if output := result.Output[""]; output != nil {
			report.WriteString(output.String())
		}
		report.Write(stderr.Bytes())
		if runErr != nil && len(result.Failed) == 0 {
			fmt.Fprintf(&report, "go test: %v\n", runErr)
		}
	}
	return report.String(), unitFailed, skipped, result.Timings, nil
}
