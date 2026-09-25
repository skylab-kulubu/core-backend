package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

func TestYTUBackfillCommandIsADryRunUnlessApplied(t *testing.T) {
	store := user.NewMemoryStore()
	id := uuid.New()
	if _, _, err := user.NewService(store).Ensure(context.Background(), id, user.Profile{Email: "ada@example.com"}); err != nil {
		t.Fatal(err)
	}
	read := func(context.Context) ([]user.YTUAttributes, error) {
		return []user.YTUAttributes{
			{ID: id, University: "Yıldız Teknik Üniversitesi", Department: "011"},
			{ID: uuid.New(), University: "Yıldız Teknik Üniversitesi", Department: "0B1"},
		}, nil
	}

	var out bytes.Buffer
	if code := ytuBackfillCommand(context.Background(), nil, &out, read, store); code != 0 {
		t.Fatalf("dry run exit %d: %s", code, out.String())
	}
	for _, line := range []string{"dry run", "accounts with the YTÜ university: 2", "records to update: 1", "no core record yet (filled on first request): 1", `"0B1": 1`} {
		if !strings.Contains(out.String(), line) {
			t.Fatalf("dry run output lacks %q:\n%s", line, out.String())
		}
	}
	if got, _ := store.Get(context.Background(), id); got.YTULinked {
		t.Fatal("a dry run wrote")
	}

	out.Reset()
	if code := ytuBackfillCommand(context.Background(), []string{"-apply"}, &out, read, store); code != 0 {
		t.Fatalf("apply exit %d: %s", code, out.String())
	}
	if !strings.Contains(out.String(), "records updated: 1") {
		t.Fatalf("apply output:\n%s", out.String())
	}
	if got, _ := store.Get(context.Background(), id); !got.YTULinked || got.Department != "Bilgisayar Mühendisliği" {
		t.Fatalf("after apply %+v", got)
	}
}

func TestYTUBackfillCommandFailures(t *testing.T) {
	store := user.NewMemoryStore()
	var out bytes.Buffer
	if code := ytuBackfillCommand(context.Background(), []string{"-nope"}, &out, nil, store); code != 2 {
		t.Fatalf("unknown flag exit %d", code)
	}
	failing := func(context.Context) ([]user.YTUAttributes, error) { return nil, errors.New("keycloak down") }
	out.Reset()
	if code := ytuBackfillCommand(context.Background(), nil, &out, failing, store); code != 1 || !strings.Contains(out.String(), "keycloak down") {
		t.Fatalf("keycloak failure exit %d: %s", code, out.String())
	}
}

func TestYTUBackfillNeedsTheCoreEnvironment(t *testing.T) {
	var out bytes.Buffer
	env := map[string]string{"DATABASE_URL": "postgres://x", "KEYCLOAK_URL": "https://e.example.test", "KEYCLOAK_REALM": "e-skylab", "KEYCLOAK_CLIENT_ID": "core"}
	if code := runYTUBackfill(nil, func(k string) string { return env[k] }, &out); code != 2 || !strings.Contains(out.String(), "KEYCLOAK_CLIENT_SECRET") {
		t.Fatalf("exit %d: %s", code, out.String())
	}
	if strings.Contains(out.String(), "postgres://x") {
		t.Fatal("the output echoes configuration values")
	}
}
