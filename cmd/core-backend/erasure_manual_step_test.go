package main

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

func TestErasureManualStepOptions(t *testing.T) {
	id := "0b6f2a3e-1c2d-4e5f-8a9b-0c1d2e3f4a5b"
	for _, tc := range []struct {
		name   string
		args   []string
		step   user.DeletionStep
		counts map[string]int64
		code   int
		errOut string
	}{
		{name: "cms", args: []string{"--request", id, "--service", "cms", "--counts", `{"actor_columns_replaced":2}`},
			step: user.DeletionStepEraseCMS, counts: map[string]int64{"actor_columns_replaced": 2}},
		{name: "forms, nothing found", args: []string{"--request", " " + strings.ToUpper(id) + " ", "--service", " forms ", "--counts", `{}`},
			step: user.DeletionStepEraseForms, counts: map[string]int64{}},
		{name: "no request", args: []string{"--service", "cms", "--counts", `{}`}, code: 2, errOut: "--request"},
		{name: "bad request", args: []string{"--request", "nope", "--service", "cms", "--counts", `{}`}, code: 2, errOut: "--request"},
		{name: "no service", args: []string{"--request", id, "--counts", `{}`}, code: 2, errOut: "--service"},
		{name: "unknown service", args: []string{"--request", id, "--service", "core", "--counts", `{}`}, code: 2, errOut: "skymail, cms or forms"},
		{name: "no counts", args: []string{"--request", id, "--service", "cms"}, code: 2, errOut: "--counts"},
		{name: "counts not an object", args: []string{"--request", id, "--service", "cms", "--counts", `[1]`}, code: 2, errOut: "counts is not an object"},
		{name: "negative count", args: []string{"--request", id, "--service", "cms", "--counts", `{"rows":-1}`}, code: 2, errOut: "non-negative"},
		{name: "address as key", args: []string{"--request", id, "--service", "cms", "--counts", `{"ada@example.com":1}`}, code: 2, errOut: "snake_case"},
		{name: "operator key", args: []string{"--request", id, "--service", "cms", "--counts", `{"operator_recorded":1}`}, code: 2, errOut: "operator_recorded"},
		{name: "stray argument", args: []string{"--request", id, "--service", "cms", "--counts", `{}`, "extra"}, code: 2, errOut: "argument"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var errOut bytes.Buffer
			got, code := parseErasureManualStepOptions(tc.args, &errOut)
			if code != tc.code {
				t.Fatalf("code = %d, want %d (%s)", code, tc.code, errOut.String())
			}
			if !strings.Contains(errOut.String(), tc.errOut) {
				t.Fatalf("errOut = %q, want %q", errOut.String(), tc.errOut)
			}
			if strings.Contains(errOut.String(), "ada@") {
				t.Fatalf("the error quotes the counts: %s", errOut.String())
			}
			if code != 0 {
				return
			}
			if got.requestID.String() != id || got.service.Step != tc.step || !maps.Equal(got.counts, tc.counts) {
				t.Fatalf("options = %+v", got)
			}
		})
	}
}

// The command hands the store the request, the step and the counts, prints
// the record (request id, step, time, counts; nothing about the person) and
// gives every outcome its exit code.
func TestErasureManualStepCommand(t *testing.T) {
	now := time.Date(2026, 10, 27, 20, 0, 0, 0, time.UTC)
	id := uuid.MustParse("0b6f2a3e-1c2d-4e5f-8a9b-0c1d2e3f4a5b")
	var errOut bytes.Buffer
	options, code := parseErasureManualStepOptions([]string{"--request", id.String(), "--service", "cms", "--counts", `{"actor_columns_replaced":2}`}, &errOut)
	if code != 0 {
		t.Fatalf("options: %s", errOut.String())
	}
	for _, tc := range []struct {
		name   string
		err    error
		code   int
		out    []string
		errOut string
	}{
		{name: "recorded", out: []string{"request_id: " + id.String(), "step: erase_cms", "at: 2026-10-27T20:00:00Z",
			`counts: {"actor_columns_replaced":2,"operator_recorded":1}`, "back to pending"}},
		{name: "not waiting", err: user.ErrOperatorStepRefused, code: erasureManualStepExitRefused, errOut: "nothing changed"},
		{name: "unknown", err: user.ErrNotFound, code: erasureManualStepExitRefused, errOut: "no deletion request"},
		{name: "database", err: errors.New("connection refused"), code: 1, errOut: "nothing changed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			called := false
			code := erasureManualStepCommand(context.Background(), &out, &errOut, now, options,
				func(_ context.Context, requestID uuid.UUID, step user.DeletionStep, at time.Time, counts map[string]int64) error {
					called = true
					if requestID != id || step != user.DeletionStepEraseCMS || !at.Equal(now) || !maps.Equal(counts, map[string]int64{"actor_columns_replaced": 2}) {
						t.Errorf("record(%s, %s, %s, %v)", requestID, step, at, counts)
					}
					return tc.err
				})
			if code != tc.code || !called {
				t.Fatalf("code = %d called=%v, want %d (out %q, errOut %q)", code, called, tc.code, out.String(), errOut.String())
			}
			for _, want := range tc.out {
				if !strings.Contains(out.String(), want) {
					t.Fatalf("out = %q, want %q", out.String(), want)
				}
			}
			if tc.err != nil && out.Len() != 0 {
				t.Fatalf("a refused or failed record printed %q", out.String())
			}
			if !strings.Contains(errOut.String(), tc.errOut) {
				t.Fatalf("errOut = %q, want %q", errOut.String(), tc.errOut)
			}
		})
	}
}

func TestErasureManualStepNeedsTheDatabase(t *testing.T) {
	var out, errOut bytes.Buffer
	args := []string{"--request", uuid.NewString(), "--service", "cms", "--counts", `{}`}
	if code := runErasureManualStep(args, func(string) string { return "" }, &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "DATABASE_URL") {
		t.Fatalf("code = %d errOut = %q", code, errOut.String())
	}
}
