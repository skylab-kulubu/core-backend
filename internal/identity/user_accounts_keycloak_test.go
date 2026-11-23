package identity_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/identity"
)

// accountKeycloak answers the person card's Admin REST calls from fixed
// bodies and keeps every request (method, path with query, body, erasure
// header).
type accountKeycloak struct {
	mu       sync.Mutex
	id       uuid.UUID
	answers  map[string]accountAnswer
	requests []accountRequest
}

type accountAnswer struct {
	status int
	body   string
}

type accountRequest struct {
	method, path, body, erasure string
}

func (f *accountKeycloak) serve(t *testing.T) *identity.Keycloak {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/protocol/openid-connect/token") {
			_, _ = w.Write([]byte(`{"access_token":"tok","expires_in":300,"token_type":"Bearer"}`))
			return
		}
		body, _ := io.ReadAll(r.Body)
		path := strings.TrimPrefix(r.URL.Path, "/admin/realms/e-skylab")
		key := r.Method + " " + path
		f.mu.Lock()
		f.requests = append(f.requests, accountRequest{method: r.Method, path: r.URL.RequestURI(), body: string(body), erasure: r.Header.Get(identity.AccountErasureHeader)})
		answer, ok := f.answers[key]
		f.mu.Unlock()
		if !ok {
			answer = accountAnswer{status: http.StatusNoContent}
		}
		w.WriteHeader(answer.status)
		_, _ = w.Write([]byte(answer.body))
	}))
	t.Cleanup(srv.Close)
	return identity.NewKeycloak(identity.KeycloakConfig{URL: srv.URL, Realm: "e-skylab", ClientID: "core", ClientSecret: "secret"})
}

func (f *accountKeycloak) last() accountRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[len(f.requests)-1]
}

func newAccountKeycloak() *accountKeycloak {
	id := uuid.New()
	user := `{"id":"` + id.String() + `","username":"ada","email":"ada@example.com","emailVerified":false,"firstName":"Ada","lastName":"Lovelace","enabled":true,"createdTimestamp":1760000000000,"requiredActions":["CONFIGURE_TOTP"],"attributes":{"schoolEmail":["ada@std.yildiz.edu.tr"],"personalEmail":["ada@example.com"],"skyNumber":["SKY-1"]}}`
	return &accountKeycloak{id: id, answers: map[string]accountAnswer{
		"GET /users/" + id.String():                              {200, user},
		"GET /users/" + id.String() + "/credentials":             {200, `[{"id":"c1","type":"otp","userLabel":"Telefon","createdDate":1760000000000,"credentialData":"{\"secret\":\"x\"}","secretData":"s"}]`},
		"GET /users/" + id.String() + "/federated-identity":      {200, `[{"identityProvider":"OBS","userId":"ms-1","userName":"ada@std.yildiz.edu.tr"}]`},
		"GET /users/" + id.String() + "/sessions":                {200, `[{"id":"s1","ipAddress":"203.0.113.9","start":1760000000000,"lastAccess":1760000600000,"clients":{"u1":"admin","u2":"account-center"}}]`},
		"GET /attack-detection/brute-force/users/" + id.String(): {200, `{"numFailures":3,"disabled":true,"lastIPFailure":"203.0.113.9","lastFailure":1760000600000}`},
		"GET /events": {200, `[{"time":1760000600000,"type":"LOGIN","clientId":"admin","ipAddress":"203.0.113.9"},{"time":1760000700000,"type":"LOGIN_ERROR","clientId":"admin","error":"invalid_user_credentials","ipAddress":"203.0.113.9"}]`},
	}}
}

func TestKeycloakAccountReadsLeaveSecretsAndAddressesOut(t *testing.T) {
	f := newAccountKeycloak()
	k := f.serve(t)
	ctx := context.Background()
	u, err := k.AccountUser(ctx, f.id)
	if err != nil || u.Username != "ada" || u.EmailVerified || !u.Enabled || u.CreatedAt == nil || u.RequiredActions[0] != "CONFIGURE_TOTP" {
		t.Fatalf("user = %+v %v", u, err)
	}
	credentials, err := k.AccountCredentials(ctx, f.id)
	if err != nil || len(credentials) != 1 || credentials[0].Label != "Telefon" {
		t.Fatalf("credentials = %+v %v", credentials, err)
	}
	sessions, err := k.AccountSessions(ctx, f.id)
	if err != nil || len(sessions) != 1 || strings.Join(sessions[0].Clients, ",") != "account-center,admin" {
		t.Fatalf("sessions = %+v %v", sessions, err)
	}
	lockout, err := k.Lockout(ctx, f.id)
	if err != nil || !lockout.Locked || lockout.Failures != 3 {
		t.Fatalf("lockout = %+v %v", lockout, err)
	}
	events, err := k.LoginEvents(ctx, f.id, 10)
	if err != nil || len(events) != 2 || events[0].Type != "LOGIN_ERROR" || events[0].Error != "invalid_user_credentials" {
		t.Fatalf("events = %+v %v", events, err)
	}
	if q := f.last().path; !strings.Contains(q, "user="+f.id.String()) || !strings.Contains(q, "type=LOGIN") || !strings.Contains(q, "max=10") {
		t.Fatalf("events query = %s", q)
	}
	raw, _ := json.Marshal([]any{credentials, sessions, lockout, events})
	for _, secret := range []string{"203.0.113.9", "secret", "credentialData"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("%q read out: %s", secret, raw)
		}
	}

	f.answers["GET /events"] = accountAnswer{403, `{"error":"unknown_error"}`}
	if _, err := k.LoginEvents(ctx, f.id, 10); !errors.Is(err, identity.ErrEventsUnavailable) {
		t.Fatalf("without view-events: %v", err)
	}
}

func TestKeycloakEnableCarriesNoErasureMark(t *testing.T) {
	f := newAccountKeycloak()
	k := f.serve(t)
	if err := k.SetEnabled(context.Background(), f.id, false); err != nil {
		t.Fatal(err)
	}
	got := f.last()
	if got.method != http.MethodPut || got.body != `{"enabled":false}` || got.erasure != "" {
		t.Fatalf("request = %+v", got)
	}
}

func TestKeycloakTemporaryPasswordNamesNoPasswordInErrors(t *testing.T) {
	f := newAccountKeycloak()
	k := f.serve(t)
	if err := k.SetTemporaryPassword(context.Background(), f.id, "Gizli-Parola-1"); err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(f.last().body), &body); err != nil || body["temporary"] != true || body["type"] != "password" || body["value"] != "Gizli-Parola-1" {
		t.Fatalf("body = %s", f.last().body)
	}
	if !strings.HasSuffix(f.last().path, "/reset-password") {
		t.Fatalf("path = %s", f.last().path)
	}
	f.answers["PUT /users/"+f.id.String()+"/reset-password"] = accountAnswer{400, `{"error":"invalidPasswordMinLengthMessage","error_description":"Invalid password: minimum length 8. Gizli"}`}
	err := k.SetTemporaryPassword(context.Background(), f.id, "Gizli")
	var refused *identity.AccountError
	if !errors.As(err, &refused) || refused.Policy != "invalidPasswordMinLengthMessage" || strings.Contains(err.Error(), "Gizli") {
		t.Fatalf("refused = %v", err)
	}
}

func TestKeycloakProfileWriteSendsTheProfileWhole(t *testing.T) {
	f := newAccountKeycloak()
	k := f.serve(t)
	address := "yeni@example.com"
	if err := k.UpdateAccount(context.Background(), f.id, identity.AccountUpdate{
		Email: &address, EmailVerified: new(bool),
		Attributes: map[string]string{"personalEmail": address, "personalEmailVerifiedAt": ""},
	}); err != nil {
		t.Fatal(err)
	}
	var body struct {
		Username, Email, FirstName, LastName string
		EmailVerified                        *bool `json:"emailVerified"`
		Enabled                              *bool `json:"enabled"`
		Attributes                           map[string][]string
	}
	if err := json.Unmarshal([]byte(f.last().body), &body); err != nil {
		t.Fatal(err)
	}
	if body.Username != "ada" || body.Email != address || body.FirstName != "Ada" || body.LastName != "Lovelace" ||
		body.EmailVerified == nil || *body.EmailVerified || body.Enabled != nil {
		t.Fatalf("body = %s", f.last().body)
	}
	if body.Attributes["personalEmail"][0] != address || body.Attributes["skyNumber"][0] != "SKY-1" || body.Attributes["schoolEmail"][0] != "ada@std.yildiz.edu.tr" {
		t.Fatalf("attributes = %v", body.Attributes)
	}
	if _, ok := body.Attributes["personalEmailVerifiedAt"]; ok {
		t.Fatal("the stamp stayed")
	}

	// A flag alone leaves the profile out.
	actions := []string{"UPDATE_PASSWORD"}
	if err := k.UpdateAccount(context.Background(), f.id, identity.AccountUpdate{RequiredActions: &actions}); err != nil {
		t.Fatal(err)
	}
	if got := f.last().body; got != `{"requiredActions":["UPDATE_PASSWORD"]}` {
		t.Fatalf("flag body = %s", got)
	}
	f.answers["PUT /users/"+f.id.String()] = accountAnswer{409, `{"errorMessage":"User exists with same email"}`}
	if err := k.UpdateAccount(context.Background(), f.id, identity.AccountUpdate{Email: &address}); accountCode(err) != identity.AccountEmailTaken {
		t.Fatalf("taken: %v", err)
	}
}

func TestKeycloakMailsAndRemovals(t *testing.T) {
	f := newAccountKeycloak()
	k := f.serve(t)
	ctx := context.Background()
	if err := k.SendActionsMail(ctx, f.id, []string{"UPDATE_PASSWORD"}); err != nil || f.last().body != `["UPDATE_PASSWORD"]` || !strings.HasSuffix(f.last().path, "/execute-actions-email") {
		t.Fatalf("actions mail: %v %+v", err, f.last())
	}
	if err := k.SendVerificationMail(ctx, f.id); err != nil || !strings.HasSuffix(f.last().path, "/send-verify-email") {
		t.Fatalf("verify mail: %v %+v", err, f.last())
	}
	f.answers["PUT /users/"+f.id.String()+"/send-verify-email"] = accountAnswer{500, `{"errorMessage":"Failed to send verify email"}`}
	if err := k.SendVerificationMail(ctx, f.id); accountCode(err) != identity.AccountMailFailed {
		t.Fatalf("failed mail: %v", err)
	}
	if err := k.DeleteCredential(ctx, f.id, "c1"); err != nil || f.last().method != http.MethodDelete || !strings.HasSuffix(f.last().path, "/credentials/c1") {
		t.Fatalf("credential: %v %+v", err, f.last())
	}
	if err := k.UnlinkIdentity(ctx, f.id, "OBS"); err != nil || !strings.HasSuffix(f.last().path, "/federated-identity/OBS") {
		t.Fatalf("unlink: %v %+v", err, f.last())
	}
	if err := k.ClearLockout(ctx, f.id); err != nil || f.last().path != "/admin/realms/e-skylab/attack-detection/brute-force/users/"+f.id.String() {
		t.Fatalf("lockout: %v %+v", err, f.last())
	}
	f.answers["DELETE /users/"+f.id.String()+"/credentials/c2"] = accountAnswer{404, `{}`}
	if err := k.DeleteCredential(ctx, f.id, "c2"); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("missing credential: %v", err)
	}
}

// A refused write is named by what it wrote: a name Keycloak's validators
// refuse is no e-mail problem.
func TestKeycloakRefusalNamesTheField(t *testing.T) {
	f := newAccountKeycloak()
	k := f.serve(t)
	f.answers["PUT /users/"+f.id.String()] = accountAnswer{400, `{"field":"firstName","errorMessage":"error-person-name-invalid-character"}`}
	name := "Ada<"
	if err := k.UpdateAccount(context.Background(), f.id, identity.AccountUpdate{FirstName: &name}); accountCode(err) != identity.AccountNameInvalid {
		t.Fatalf("name: %v", err)
	}
	actions := []string{"UPDATE_PASSWORD"}
	if err := k.UpdateAccount(context.Background(), f.id, identity.AccountUpdate{RequiredActions: &actions}); accountCode(err) != identity.AccountInvalid {
		t.Fatalf("required actions: %v", err)
	}
	address := "x@example.com"
	if err := k.UpdateAccount(context.Background(), f.id, identity.AccountUpdate{Email: &address}); accountCode(err) != identity.AccountEmailInvalid {
		t.Fatalf("e-mail: %v", err)
	}
}
