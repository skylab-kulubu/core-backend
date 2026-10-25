package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/events"
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
			s := keycloakEvents(func() { woken++ }, func(format string, args ...any) {
				logs = append(logs, fmt.Sprintf(format, args...))
			})
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
	s := keycloakEvents(nil, func(string, ...any) {})
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
	s := keycloakEvents(func() {}, func(string, ...any) {})
	err := s.Handle(context.Background(), nil, events.Event{ID: "1", Source: "/forms", Type: "forms.form.updated"})
	if !events.IsPermanent(err) {
		t.Fatalf("err = %v", err)
	}
}
