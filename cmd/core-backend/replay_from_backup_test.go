package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/erasure"
	"github.com/skylab-kulubu/core-backend/internal/erasurereplay"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

func TestReplayOptionsRefuseWhatCannotBeReplayed(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	valid := []string{"--service", "skymail", "--dumped-at", "2026-09-20T03:00:00+03:00"}
	options, code := parseReplayOptions(valid, &bytes.Buffer{}, now)
	if code != 0 || options.service.Step != user.DeletionStepEraseSkyMail || options.apply || options.realm != "e-skylab" ||
		!options.dumpedAt.Equal(time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("options = %+v, code %d", options, code)
	}
	if options, _ := parseReplayOptions(append(valid, "-apply"), &bytes.Buffer{}, now); !options.apply {
		t.Fatal("-apply not read")
	}

	with := func(flag, value string) []string {
		args := append([]string(nil), valid...)
		for i := range args {
			if args[i] == flag {
				args[i+1] = value
			}
		}
		return args
	}
	and := func(extra ...string) []string { return append(append([]string(nil), valid...), extra...) }
	for name, args := range map[string][]string{
		"unknown service":    with("--service", "mail"),
		"no dump time":       with("--dumped-at", ""),
		"not RFC 3339":       with("--dumped-at", "2026-09-20 03:00"),
		"dump in the future": with("--dumped-at", "2026-09-28T00:00:00Z"),
		"empty realm":        and("--keycloak-realm", ""),
		"a stray argument":   and("now"),
		"an unknown flag":    and("--force"),
		// A DSN carries a password: it never goes on the command line.
		"a DSN in argv": and("--core-snapshot-dsn", "postgres://replay:pw@core-snap/core"),
	} {
		var out bytes.Buffer
		if _, code := parseReplayOptions(args, &out, now); code != 2 {
			t.Fatalf("%s: exit %d\n%s", name, code, out.String())
		}
	}
	// T is when the service dump was taken; the old name invited the restore
	// time, which misses the requests completed in between. It is gone, and
	// passing it points at the new one.
	for _, old := range [][]string{
		{"--service", "skymail", "--restored-at", "2026-09-20T03:00:00Z"},
		{"--service", "skymail", "-restored-at=2026-09-20T03:00:00Z"},
		append(append([]string(nil), valid...), "--restored-at", "2026-09-20T03:00:00Z"),
	} {
		var out bytes.Buffer
		if _, code := parseReplayOptions(old, &out, now); code != 2 ||
			!strings.Contains(out.String(), "--restored-at is now --dumped-at: T is when the service dump was taken") {
			t.Fatalf("%q: exit %d\n%s", old, code, out.String())
		}
	}
	var out bytes.Buffer
	parseReplayOptions([]string{"-h"}, &out, now)
	for _, want := range []string{"CORE_SNAPSHOT_DATABASE_URL=<dsn> KEYCLOAK_SNAPSHOT_DATABASE_URL=<dsn> core-backend replay-from-backup", "-apply", "-dumped-at"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("usage lacks %q:\n%s", want, out.String())
		}
	}
	if !strings.Contains(out.String(), "T is when the service dump was taken") {
		t.Fatalf("usage does not say what T is:\n%s", out.String())
	}
	if strings.Contains(out.String(), "snapshot-dsn") || strings.Contains(out.String(), "restored-at") {
		t.Fatalf("usage still offers a DSN flag:\n%s", out.String())
	}
}

// Every DSN holds a user, a password, a host and a database; no output may
// carry any of them.
var replayDSNParts = []string{"live-user", "live-pass", "live_db", "core-user", "core-pass", "core_db", "kc-user", "kc-pass", "kc_db", "127.0.0.1", "s3cr3t"}

func replayEnv() map[string]string {
	return map[string]string{
		"DATABASE_URL":                   "postgres://live-user:live-pass@127.0.0.1:1/live_db",
		"CORE_SNAPSHOT_DATABASE_URL":     "postgres://core-user:core-pass@127.0.0.1:1/core_db",
		"KEYCLOAK_SNAPSHOT_DATABASE_URL": "postgres://kc-user:kc-pass@127.0.0.1:1/kc_db",
		"KEYCLOAK_URL":                   "http://keycloak:8080",
		"KEYCLOAK_REALM":                 "e-skylab",
		"ACCOUNT_ERASURE_CLIENT_ID":      "core-erasure",
		"ACCOUNT_ERASURE_CLIENT_SECRET":  "s3cr3t-value-never-printed",
	}
}

func TestReplayReadsTheSnapshotsFromTheEnvironmentAndNamesOnlyVariables(t *testing.T) {
	t.Parallel()

	dryRun := []string{"--service", "cms", "--dumped-at", "2026-09-20T03:00:00Z"}
	apply := append(append([]string(nil), dryRun...), "--apply")
	for _, tc := range []struct {
		name   string
		args   []string
		change func(env map[string]string)
		code   int
		want   string
	}{
		{name: "apply without the service URL", args: apply, code: 2, want: "--apply: ACCOUNT_ERASURE_CMS_URL is required"},
		{name: "no DSNs", args: dryRun, code: 2,
			change: func(env map[string]string) {
				delete(env, "DATABASE_URL")
				delete(env, "CORE_SNAPSHOT_DATABASE_URL")
				env["KEYCLOAK_SNAPSHOT_DATABASE_URL"] = " "
			},
			want: "replay-from-backup needs DATABASE_URL, CORE_SNAPSHOT_DATABASE_URL, KEYCLOAK_SNAPSHOT_DATABASE_URL"},
		{name: "a snapshot is the live database", args: dryRun, code: 2,
			change: func(env map[string]string) { env["KEYCLOAK_SNAPSHOT_DATABASE_URL"] = env["DATABASE_URL"] },
			want:   "CORE_SNAPSHOT_DATABASE_URL or KEYCLOAK_SNAPSHOT_DATABASE_URL is DATABASE_URL"},
		{name: "one database for both snapshots", args: dryRun, code: 2,
			change: func(env map[string]string) { env["KEYCLOAK_SNAPSHOT_DATABASE_URL"] = env["CORE_SNAPSHOT_DATABASE_URL"] },
			want:   "CORE_SNAPSHOT_DATABASE_URL and KEYCLOAK_SNAPSHOT_DATABASE_URL name the same database"},
		{name: "live database unreachable", args: dryRun, code: 1, want: "DATABASE_URL: cannot connect\n"},
		{name: "live DSN unparsable", args: dryRun, code: 1,
			change: func(env map[string]string) { env["DATABASE_URL"] = "postgres://live-user:live-pass@[127.0.0.1/live_db" },
			want:   "DATABASE_URL: the DSN cannot be parsed\n"},
	} {
		env := replayEnv()
		if tc.change != nil {
			tc.change(env)
		}
		var out bytes.Buffer
		code := runReplayFromBackup(tc.args, func(key string) string { return env[key] }, &out)
		if code != tc.code || !strings.Contains(out.String(), tc.want) {
			t.Fatalf("%s: exit %d, want %d and %q:\n%s", tc.name, code, tc.code, tc.want, out.String())
		}
		for _, part := range replayDSNParts {
			if strings.Contains(out.String(), part) {
				t.Fatalf("%s: output carries %q:\n%s", tc.name, part, out.String())
			}
		}
	}
}

func TestKeycloakTokenURLIsDerivedAsTheServerDerivesIt(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		url, realm, want string
	}{
		{"http://keycloak:8080/", "e-skylab", "http://keycloak:8080/realms/e-skylab/protocol/openid-connect/token"},
		{"https://auth.example/realms/e-skylab", "", "https://auth.example/realms/e-skylab/protocol/openid-connect/token"},
		{"", "e-skylab", ""},
		{"http://keycloak:8080", "", ""},
	} {
		env := map[string]string{"KEYCLOAK_URL": tc.url, "KEYCLOAK_REALM": tc.realm}
		got, ok := keycloakTokenURL(func(key string) string { return env[key] })
		if got != tc.want || ok != (tc.want != "") {
			t.Fatalf("%q %q = %q %v", tc.url, tc.realm, got, ok)
		}
	}
}

type stubRequests struct {
	completed []erasurereplay.Request
	open      []uuid.UUID
}

func (s stubRequests) CompletedSince(context.Context, time.Time) ([]erasurereplay.Request, error) {
	return s.completed, nil
}

func (s stubRequests) OpenWithStepSince(context.Context, user.DeletionStep, time.Time) ([]uuid.UUID, error) {
	return s.open, nil
}

type stubCore struct{ known map[uuid.UUID]bool }

func (stubCore) Check(context.Context) error { return nil }

func (s stubCore) Get(_ context.Context, id uuid.UUID) (user.User, error) {
	if !s.known[id] {
		return user.User{}, user.ErrNotFound
	}
	return user.User{ID: id, Email: "person@example.com"}, nil
}

type stubKeycloak struct{}

func (stubKeycloak) Check(context.Context) error { return nil }

func (stubKeycloak) UserAddresses(context.Context, uuid.UUID) ([]string, error) {
	return nil, identity.ErrNotFound
}

type stubSender struct{ err error }

func (s stubSender) Erase(context.Context, erasure.Command) (erasure.Result, error) {
	return erasure.Result{Counts: map[string]int64{}}, s.err
}

func TestReplayExitsNonZeroOnAnyFailAndAsksForAnotherRunWhenUnfinished(t *testing.T) {
	t.Parallel()

	service, _ := erasure.ServiceNamed("skymail")
	known := erasurereplay.Request{ID: uuid.New(), SubjectID: uuid.New()}
	missing := erasurereplay.Request{ID: uuid.New(), SubjectID: uuid.New()}
	inProgress := &erasure.DeferredError{Step: service.Step, Reason: "in progress (202)", Status: 202}
	down := &erasure.DeferredError{Step: service.Step, Reason: "service unavailable (503)", Status: 503}
	for _, tc := range []struct {
		name      string
		requests  stubRequests
		apply     bool
		sendError error
		want      int
	}{
		{name: "dry run, all resolved", requests: stubRequests{completed: []erasurereplay.Request{known}}, want: 0},
		{name: "dry run, a subject missing", requests: stubRequests{completed: []erasurereplay.Request{known, missing}}, want: 1},
		{name: "nothing to replay", want: 0},
		{name: "done", requests: stubRequests{completed: []erasurereplay.Request{known}}, apply: true, want: 0},
		{name: "202", requests: stubRequests{completed: []erasurereplay.Request{known}}, apply: true, sendError: inProgress, want: replayExitRetryLater},
		{name: "503", requests: stubRequests{completed: []erasurereplay.Request{known}}, apply: true, sendError: down, want: 1},
		{name: "open request", requests: stubRequests{open: []uuid.UUID{uuid.New()}}, apply: true, want: replayExitRetryLater},
		{name: "202 and a missing subject", requests: stubRequests{completed: []erasurereplay.Request{known, missing}}, apply: true, sendError: inProgress, want: 1},
	} {
		var out bytes.Buffer
		code := replayFromBackupCommand(context.Background(), &out, erasurereplay.Replay{
			Service: service, DumpedAt: time.Date(2026, 9, 20, 3, 0, 0, 0, time.UTC),
			Requests: tc.requests, Core: stubCore{known: map[uuid.UUID]bool{known.SubjectID: true}}, Keycloak: stubKeycloak{},
			Apply: tc.apply, Sender: stubSender{err: tc.sendError},
		})
		if code != tc.want {
			t.Fatalf("%s: exit %d, want %d\n%s", tc.name, code, tc.want, out.String())
		}
		if strings.Contains(out.String(), "person@example.com") || strings.Contains(out.String(), known.SubjectID.String()) {
			t.Fatalf("%s: output carries personal data:\n%s", tc.name, out.String())
		}
	}
}
