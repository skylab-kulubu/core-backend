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
	valid := []string{"--service", "skymail", "--restored-at", "2026-09-20T03:00:00+03:00",
		"--core-snapshot-dsn", "postgres://core-snap", "--keycloak-snapshot-dsn", "postgres://kc-snap"}
	options, code := parseReplayOptions(valid, &bytes.Buffer{}, now)
	if code != 0 || options.service.Step != user.DeletionStepEraseSkyMail || options.apply || options.realm != "e-skylab" ||
		!options.restoredAt.Equal(time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)) {
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
	for name, args := range map[string][]string{
		"unknown service":       with("--service", "mail"),
		"no restore time":       with("--restored-at", ""),
		"not RFC 3339":          with("--restored-at", "2026-09-20 03:00"),
		"restore in the future": with("--restored-at", "2026-09-28T00:00:00Z"),
		"no core snapshot":      with("--core-snapshot-dsn", ""),
		"no keycloak snapshot":  with("--keycloak-snapshot-dsn", " "),
		"one database for both": with("--keycloak-snapshot-dsn", "postgres://core-snap"),
		"empty realm":           append(append([]string(nil), valid...), "--keycloak-realm", ""),
		"a stray argument":      append(append([]string(nil), valid...), "now"),
		"an unknown flag":       append(append([]string(nil), valid...), "--force"),
	} {
		var out bytes.Buffer
		if _, code := parseReplayOptions(args, &out, now); code != 2 {
			t.Fatalf("%s: exit %d\n%s", name, code, out.String())
		}
	}
}

func TestReplayNamesAMissingVariableNeverAValue(t *testing.T) {
	t.Parallel()

	args := []string{"--service", "cms", "--restored-at", "2026-09-20T03:00:00Z",
		"--core-snapshot-dsn", "postgres://replay:snap-pass@core-snap/core", "--keycloak-snapshot-dsn", "postgres://replay:snap-pass@kc-snap/keycloak", "--apply"}
	env := map[string]string{
		"DATABASE_URL":                  "postgres://core:db-pass-never-printed@127.0.0.1:1/core",
		"KEYCLOAK_URL":                  "http://keycloak:8080",
		"KEYCLOAK_REALM":                "e-skylab",
		"ACCOUNT_ERASURE_CLIENT_ID":     "core-erasure",
		"ACCOUNT_ERASURE_CLIENT_SECRET": "s3cr3t-value-never-printed",
	}
	var out bytes.Buffer
	if code := runReplayFromBackup(args, func(key string) string { return env[key] }, &out); code != 2 {
		t.Fatalf("exit %d\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "ACCOUNT_ERASURE_CMS_URL is required") {
		t.Fatalf("output:\n%s", out.String())
	}
	for _, value := range []string{"db-pass", "snap-pass", "s3cr3t"} {
		if strings.Contains(out.String(), value) {
			t.Fatalf("output carries %q:\n%s", value, out.String())
		}
	}

	env["DATABASE_URL"] = "postgres://replay:snap-pass@core-snap/core"
	out.Reset()
	if code := runReplayFromBackup(args, func(key string) string { return env[key] }, &out); code != 2 ||
		!strings.Contains(out.String(), "a snapshot DSN is DATABASE_URL") || strings.Contains(out.String(), "snap-pass") {
		t.Fatalf("a snapshot pointing at the live database: exit %d\n%s", code, out.String())
	}

	delete(env, "DATABASE_URL")
	out.Reset()
	if code := runReplayFromBackup(args, func(key string) string { return env[key] }, &out); code != 2 ||
		!strings.Contains(out.String(), "needs DATABASE_URL") {
		t.Fatalf("no DATABASE_URL: exit %d\n%s", code, out.String())
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
			Service: service, RestoredAt: time.Date(2026, 9, 20, 3, 0, 0, 0, time.UTC),
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
