package handlers

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
)

func TestGroupDeleteHTTPContract(t *testing.T) {
	dir := identity.NewMemory()
	root := identity.Group{ID: uuid.NewString(), Name: "ADMIN", Path: "/ADMIN"}
	dir.PutGroup(root)
	_ = dir.SetGroupClientRoles(context.Background(), root.ID, []identity.ClientRole{{ClientID: "core", Role: "groups:manage"}})
	group := identity.Group{ID: uuid.NewString(), Name: "TANISMAYEMEGIGRUBU", Path: "/TANISMAYEMEGIGRUBU"}
	dir.PutGroup(group)
	person := uuid.New()
	dir.PutUser(identity.Person{ID: person})
	_ = dir.AddMember(context.Background(), group.ID, person)
	audit := teamaudit.NewMemory()
	app := auditedApp(t, authn.Identity{ID: uuid.New(), Groups: []string{"/ADMIN"}}, dir, audit)
	resp, err := app.Test(httptest.NewRequest("DELETE", "/v1/groups/"+group.ID, nil))
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 409 || body["code"] != "not_empty" || body["members"] != float64(1) || body["children"] != float64(0) {
		t.Fatalf("status%d body%v", resp.StatusCode, body)
	}
	_ = dir.RemoveMember(context.Background(), group.ID, person)
	for _, want := range []int{204, 404} {
		resp, err := app.Test(httptest.NewRequest("DELETE", "/v1/groups/"+group.ID, nil))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("delete status%d want%d", resp.StatusCode, want)
		}
	}
	if len(audit.Rows()) != 2 {
		t.Fatalf("audit rows %d", len(audit.Rows()))
	}
}
