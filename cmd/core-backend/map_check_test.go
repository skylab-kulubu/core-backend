package main

import (
	"bytes"
	"testing"
)

// map-check wants an http(s) archive address and a point; anything else is
// a usage error (2), before any request.
func TestMapCheckUsage(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{}, {"-url", "ftp://x/y"}, {"-url", "https://cdn.example.test/maps/istanbul.pmtiles", "-point", "kuzey"},
		{"-url", "https://cdn.example.test/maps/istanbul.pmtiles", "fazla"},
	} {
		var out, errs bytes.Buffer
		if code := runMapCheck(args, &out, &errs); code != 2 || out.Len() != 0 {
			t.Errorf("%q: %d %q", args, code, out.String())
		}
	}
}
