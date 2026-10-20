package handlers

import (
	"reflect"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/season"
)

func TestSeasonUpdateKeepsWhatTheBodyLeavesOut(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2027, 6, 30, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		body string
		want map[string]any // nil: the key is left out (cleared)
	}{
		{"empty body", `{}`, map[string]any{}},
		{"name alone", `{"name":"2026 Güz"}`, map[string]any{"name": "2026 Güz"}},
		{"active off alone", `{"active":false}`, map[string]any{"active": false}},
		{"end cleared", `{"endDate":null}`, map[string]any{"endDate": nil}},
		{"start moved", `{"startDate":"2026-10-01T00:00:00Z"}`, map[string]any{"startDate": "2026-10-01T00:00:00Z"}},
		{"whole body as sky-app's panel", `{"name":"2026-2027","startDate":"2026-09-01T00:00:00Z","endDate":"2027-06-30T00:00:00Z","active":true}`, map[string]any{}},
	}
	for _, tc := range cases {
		for _, method := range []string{fiber.MethodPut, fiber.MethodPatch} {
			store := season.NewMemoryStore()
			created, err := store.Create(t.Context(), season.Season{Name: "2026-2027", StartDate: &start, EndDate: &end, Active: true})
			if err != nil {
				t.Fatal(err)
			}
			app := scheduleApp(t, yk(), event.NewMemoryStore(), store)
			path := "/v1/seasons/" + created.ID.String()
			_, before := sendEventJSON(t, app, fiber.MethodGet, path, "")
			status, updated := sendEventJSON(t, app, method, path, tc.body)
			if status != fiber.StatusOK {
				t.Fatalf("%s %s: %d %v", tc.name, method, status, updated)
			}
			changed := []string{}
			for key, want := range tc.want {
				changed = append(changed, key)
				got, present := updated[key]
				if want == nil {
					if present {
						t.Fatalf("%s %s: %s = %v, want it cleared", tc.name, method, key, got)
					}
					continue
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("%s %s: %s = %#v, want %#v", tc.name, method, key, got, want)
				}
			}
			if got, want := withoutFields(updated, changed...), withoutFields(before, changed...); !reflect.DeepEqual(got, want) {
				t.Fatalf("%s %s changed a field it was not sent:\n got %v\nwant %v", tc.name, method, got, want)
			}
		}
	}
}

func TestSeasonUpdateRefusesClearingNameOrActive(t *testing.T) {
	t.Parallel()
	store := season.NewMemoryStore()
	created, err := store.Create(t.Context(), season.Season{Name: "2026-2027", Active: true})
	if err != nil {
		t.Fatal(err)
	}
	path := "/v1/seasons/" + created.ID.String()
	app := scheduleApp(t, yk(), event.NewMemoryStore(), store)
	for _, body := range []string{`{"name":""}`, `{"name":null}`, `{"active":null}`, `{"active":"evet"}`} {
		for _, method := range []string{fiber.MethodPut, fiber.MethodPatch} {
			if status, out := sendEventJSON(t, app, method, path, body); status != fiber.StatusBadRequest {
				t.Fatalf("%s %s: %d %v", method, body, status, out)
			}
		}
	}
	if status, out := sendEventJSON(t, scheduleApp(t, weblabLeader(), event.NewMemoryStore(), store), fiber.MethodPatch, path, `{"name":""}`); status != fiber.StatusForbidden {
		t.Fatalf("leader: %d %v", status, out)
	}
}
