package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/skylab-kulubu/core-backend/internal/events"
	"github.com/skylab-kulubu/core-backend/internal/identity"
)

func TestKeycloakEventsWakeTheUserSync(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		wake bool
		warn bool
	}{
		"a sign-up":                   {body: `{"id":"1","type":"REGISTER","userId":"u"}`, wake: true},
		"a new e-mail":                {body: `{"id":"1","type":"UPDATE_EMAIL","userId":"u"}`, wake: true},
		"a profile change":            {body: `{"id":"1","type":"UPDATE_PROFILE","userId":"u"}`, wake: true},
		"a deletion by the person":    {body: `{"id":"1","type":"DELETE_ACCOUNT","userId":"u"}`, wake: true, warn: true},
		"an admin's new user":         {body: `{"id":"1","resourceType":"USER","operationType":"CREATE"}`, wake: true},
		"an admin's change":           {body: `{"id":"1","resourceType":"USER","operationType":"UPDATE"}`, wake: true},
		"an admin's deletion":         {body: `{"id":"1","resourceType":"USER","operationType":"DELETE"}`, wake: true},
		"a sign-in":                   {body: `{"id":"1","type":"LOGIN","userId":"u"}`},
		"an admin's action e-mail":    {body: `{"id":"1","resourceType":"USER","operationType":"ACTION"}`},
		"an admin's group change":     {body: `{"id":"1","resourceType":"GROUP","operationType":"UPDATE"}`},
		"a sign-up that failed":       {body: `{"id":"1","type":"REGISTER","error":"invalid_registration"}`},
		"an admin change that failed": {body: `{"id":"1","resourceType":"USER","operationType":"UPDATE","error":"conflict"}`},
	} {
		t.Run(name, func(t *testing.T) {
			woken := 0
			var logs []string
			s := keycloakEvents(&keycloakHandling{wakeUserSync: func() { woken++ }, logf: func(format string, args ...any) {
				logs = append(logs, fmt.Sprintf(format, args...))
			}})
			if s.Queue != "core.keycloak" {
				t.Fatalf("queue = %q", s.Queue)
			}
			e, err := s.Decode(events.Message{Body: []byte(tc.body), ContentType: "application/json"})
			if err != nil {
				t.Fatal(err)
			}
			e.Queue = s.Queue
			err = s.Handle(context.Background(), nil, e)
			switch {
			case tc.wake && (err != nil || woken != 1):
				t.Fatalf("err %v, woken %d", err, woken)
			case !tc.wake && (!errors.Is(err, events.ErrUnrelated) || woken != 0):
				t.Fatalf("err %v, woken %d; want unrelated", err, woken)
			}
			if warned := len(logs) == 1 && strings.Contains(logs[0], `"level":"warn"`); warned != tc.warn || (!tc.warn && len(logs) != 0) {
				t.Fatalf("logs = %v", logs)
			}
		})
	}
}

// With the user sync off, every event is acked unread.
func TestKeycloakEventsWithoutTheUserSync(t *testing.T) {
	s := keycloakEvents(&keycloakHandling{logf: func(string, ...any) {}})
	e, err := s.Decode(events.Message{Body: []byte(`{"id":"1","type":"REGISTER","userId":"u"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Handle(context.Background(), nil, e); !errors.Is(err, events.ErrUnrelated) {
		t.Fatalf("err = %v", err)
	}
}

// An event of another decoder cannot be read as Keycloak's: it is permanent.
func TestKeycloakEventsRefuseOtherEvents(t *testing.T) {
	s := keycloakEvents(&keycloakHandling{wakeUserSync: func() {}, logf: func(string, ...any) {}})
	err := s.Handle(context.Background(), nil, events.Event{ID: "1", Source: "/forms", Type: "forms.form.updated"})
	if !events.IsPermanent(err) {
		t.Fatalf("err = %v", err)
	}
}

// deletionWorld is a Keycloak and a database for the deletion tests: the
// people Keycloak has, the people with an erasure request, the erased.
type deletionWorld struct {
	people    map[uuid.UUID]bool
	requests  map[uuid.UUID]bool
	readErr   error
	erased    []uuid.UUID
	userWakes int
	teamWakes int
	logs      []string
}

func (w *deletionWorld) GetUser(_ context.Context, id uuid.UUID) (identity.Person, error) {
	if w.readErr != nil {
		return identity.Person{}, w.readErr
	}
	if !w.people[id] {
		return identity.Person{}, identity.ErrNotFound
	}
	return identity.Person{ID: id}, nil
}

func (w *deletionWorld) handling() *keycloakHandling {
	return &keycloakHandling{
		realm:        "e-skylab",
		wakeUserSync: func() { w.userWakes++ },
		people:       w,
		requested: func(_ context.Context, _ pgx.Tx, id uuid.UUID) (bool, error) {
			return w.requests[id], nil
		},
		erase: func(_ context.Context, _ pgx.Tx, id uuid.UUID) (int64, error) {
			w.erased = append(w.erased, id)
			return 2, nil
		},
		wakeTeams: func() { w.teamWakes++ },
		logf: func(format string, args ...any) {
			w.logs = append(w.logs, fmt.Sprintf(format, args...))
		},
	}
}

func (w *deletionWorld) handle(t *testing.T, h *keycloakHandling, key, body string) error {
	t.Helper()
	s := keycloakEvents(h)
	e, err := s.Decode(events.Message{Body: []byte(body), ContentType: "application/json", RoutingKey: key})
	if err != nil {
		t.Fatal(err)
	}
	e.Queue = s.Queue
	return s.Handle(context.Background(), nil, e)
}

const (
	gonePerson  = "5f7c2a8e-0000-4000-8000-000000000001"
	goneGroup   = "9a1c4e7b-4444-4444-8444-000000000005"
	adminDelete = "KK.EVENT.ADMIN.e-skylab.SUCCESS.USER.DELETE"
	groupDelete = "KK.EVENT.ADMIN.e-skylab.SUCCESS.GROUP.DELETE"
)

func adminUserDelete(person string) string {
	return `{"id":"ev-1","realmId":"5b0e2d9a-2222-4222-8222-000000000003","resourceType":"USER","operationType":"DELETE","resourcePath":"users/` + person + `","time":1761213601000}`
}

func adminGroupDelete(group string) string {
	return `{"id":"ev-2","realmId":"5b0e2d9a-2222-4222-8222-000000000003","resourceType":"GROUP","operationType":"DELETE","resourcePath":"groups/` + group + `","time":1761213601000}`
}

// A person deleted in Keycloak outside the erasure saga (the console, the
// person's own account console): their preferences go at once, with a
// warning that names the person's id and the event's, never an address; the
// user sync is woken as before.
func TestKeycloakDeletionOutsideErasure(t *testing.T) {
	for name, tc := range map[string]struct{ key, body string }{
		"an admin's deletion":      {adminDelete, adminUserDelete(gonePerson)},
		"the person's own account": {"KK.EVENT.CLIENT.e-skylab.SUCCESS.account-console.DELETE_ACCOUNT", `{"id":"ev-3","type":"DELETE_ACCOUNT","userId":"` + gonePerson + `"}`},
	} {
		t.Run(name, func(t *testing.T) {
			w := &deletionWorld{}
			h := w.handling()
			if err := w.handle(t, h, tc.key, tc.body); err != nil {
				t.Fatal(err)
			}
			if len(w.erased) != 1 || w.erased[0].String() != gonePerson || w.userWakes != 1 || w.teamWakes != 0 {
				t.Fatalf("erased %v, user wakes %d, team wakes %d", w.erased, w.userWakes, w.teamWakes)
			}
			warned := false
			for _, line := range w.logs {
				if strings.Contains(line, `"event":"keycloak_user_deleted_outside_erasure"`) {
					warned = true
					if !strings.Contains(line, `"level":"warn"`) || !strings.Contains(line, gonePerson) || !strings.Contains(line, `"id":"ev-`) || strings.Contains(line, "@") {
						t.Fatalf("warning = %s", line)
					}
				}
			}
			if !warned || h.outsideErasure.Load() != 1 {
				t.Fatalf("logs %v, counted %d", w.logs, h.outsideErasure.Load())
			}
			if !strings.Contains(h.prometheus(), "skylab_keycloak_users_deleted_outside_erasure_total 1\n") {
				t.Fatalf("metrics = %s", h.prometheus())
			}
		})
	}
}

// Nothing is deleted while Keycloak still has the person, nor for a person
// whose erasure request the saga is working on (it deletes the rows in its
// own step); the user sync is still woken.
func TestKeycloakDeletionLeavesTheRows(t *testing.T) {
	person := uuid.MustParse(gonePerson)
	for name, w := range map[string]*deletionWorld{
		"still in Keycloak":    {people: map[uuid.UUID]bool{person: true}},
		"the saga's to delete": {requests: map[uuid.UUID]bool{person: true}},
		"no id in the path":    {},
	} {
		t.Run(name, func(t *testing.T) {
			body := adminUserDelete(gonePerson)
			if name == "no id in the path" {
				body = strings.Replace(body, "users/"+gonePerson, "users/"+gonePerson+"/groups/"+goneGroup, 1)
			}
			h := w.handling()
			if err := w.handle(t, h, adminDelete, body); err != nil {
				t.Fatal(err)
			}
			if len(w.erased) != 0 || len(w.logs) != 0 || w.userWakes != 1 || h.outsideErasure.Load() != 0 {
				t.Fatalf("erased %v, logs %v, wakes %d", w.erased, w.logs, w.userWakes)
			}
		})
	}
}

// Keycloak not answering (a 5xx, a timeout) is a transient error: nothing
// is deleted, the event is tried again.
func TestKeycloakDeletionWhenKeycloakFails(t *testing.T) {
	w := &deletionWorld{readErr: errors.New("keycloak: 503 Service Unavailable")}
	err := w.handle(t, w.handling(), adminDelete, adminUserDelete(gonePerson))
	if err == nil || events.IsPermanent(err) || errors.Is(err, events.ErrUnrelated) {
		t.Fatalf("err = %v, want a transient error", err)
	}
	if len(w.erased) != 0 {
		t.Fatalf("erased %v", w.erased)
	}
}

// A deleted Group wakes the communication preference cleanup's deleted
// teams part (which reads Keycloak itself), not the user sync.
func TestKeycloakGroupDeletionWakesTheTeamCleanup(t *testing.T) {
	w := &deletionWorld{}
	if err := w.handle(t, w.handling(), groupDelete, adminGroupDelete(goneGroup)); err != nil {
		t.Fatal(err)
	}
	if w.teamWakes != 1 || w.userWakes != 0 || len(w.erased) != 0 {
		t.Fatalf("team wakes %d, user wakes %d, erased %v", w.teamWakes, w.userWakes, w.erased)
	}
	// A Group's other admin events stay unrelated.
	for _, op := range []string{"CREATE", "UPDATE"} {
		body := strings.Replace(adminGroupDelete(goneGroup), `"DELETE"`, `"`+op+`"`, 1)
		if err := w.handle(t, w.handling(), "KK.EVENT.ADMIN.e-skylab.SUCCESS.GROUP."+op, body); !errors.Is(err, events.ErrUnrelated) {
			t.Fatalf("%s: err = %v", op, err)
		}
	}
	// Without the cleanup (no Keycloak) the event is acked unrecorded.
	h := w.handling()
	h.wakeTeams = nil
	if err := w.handle(t, h, groupDelete, adminGroupDelete(goneGroup)); !errors.Is(err, events.ErrUnrelated) {
		t.Fatalf("err = %v", err)
	}
}

// An event of another realm is unrelated: nothing deleted, nothing woken.
func TestKeycloakEventsOfAnotherRealm(t *testing.T) {
	w := &deletionWorld{}
	for _, tc := range []struct{ key, body string }{
		{"KK.EVENT.ADMIN.e-skylab-sandbox.SUCCESS.USER.DELETE", adminUserDelete(gonePerson)},
		{"KK.EVENT.ADMIN.master.SUCCESS.GROUP.DELETE", adminGroupDelete(goneGroup)},
		{"KK.EVENT.CLIENT.master.SUCCESS.account.REGISTER", `{"id":"1","type":"REGISTER","userId":"u"}`},
	} {
		if err := w.handle(t, w.handling(), tc.key, tc.body); !errors.Is(err, events.ErrUnrelated) {
			t.Fatalf("%s: err = %v", tc.key, err)
		}
	}
	if len(w.erased) != 0 || w.userWakes != 0 || w.teamWakes != 0 {
		t.Fatalf("erased %v, user wakes %d, team wakes %d", w.erased, w.userWakes, w.teamWakes)
	}
}

// The realm the routing key names is the provider's spelling of
// KEYCLOAK_REALM: no dots, no character outside its set, spaces as _.
func TestKeycloakRoutingRealm(t *testing.T) {
	for name, want := range map[string]string{
		"e-skylab":   "e-skylab",
		"e.skylab":   "eskylab",
		"sky lab":    "sky_lab",
		"skylab/ytü": "skylabyt",
	} {
		if got := keycloakRoutingRealm(name); got != want {
			t.Fatalf("%q: %q, want %q", name, got, want)
		}
	}
}
