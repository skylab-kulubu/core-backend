package main

import (
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
