package identity_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/identity"
)

// skyNumberKeycloak is a stateful Keycloak user for WriteSkyNumber that updates
// like Keycloak 26.7.4's admin API: a PUT changes only the fields its body
// names, replaces the attribute map as a whole and, when the body has an
// attribute map, clears the e-mail and names it leaves out (username is
// read-only with editUsernameAllowed=false). afterGet, when set, runs once
// after the first GET has been answered and before the handler returns.
type skyNumberKeycloak struct {
	mu        sync.Mutex
	id        uuid.UUID
	user      map[string]any
	getStatus int
	putStatus int
	methods   []string
	putBodies []string
	afterGet  func()
	once      sync.Once
}

func (f *skyNumberKeycloak) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/protocol/openid-connect/token") {
			_, _ = w.Write([]byte(`{"access_token":"tok","expires_in":300,"token_type":"Bearer"}`))
			return
		}
		if r.URL.Path != "/admin/realms/e-skylab/users/"+f.id.String() {
			http.NotFound(w, r)
			return
		}
		switch r.Method {
		case http.MethodGet:
			f.mu.Lock()
			f.methods = append(f.methods, r.Method)
			if f.getStatus != 0 {
				w.WriteHeader(f.getStatus)
				_, _ = w.Write([]byte(`{"error":"User not found"}`))
				f.mu.Unlock()
				return
			}
			_ = json.NewEncoder(w).Encode(f.user)
			f.mu.Unlock()
			if f.afterGet != nil {
				f.once.Do(f.afterGet)
			}
		case http.MethodPut:
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read PUT body: %v", err)
			}
			var fields map[string]any
			if err := json.Unmarshal(body, &fields); err != nil {
				t.Errorf("PUT body is not a JSON object: %v", err)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			f.methods = append(f.methods, r.Method)
			f.putBodies = append(f.putBodies, string(body))
			if f.putStatus != 0 {
				w.WriteHeader(f.putStatus)
				_, _ = w.Write([]byte(`{"error":"` + f.id.String() + ` kisi@example.com"}`))
				return
			}
			for name, value := range fields {
				f.user[name] = value
			}
			if _, ok := fields["attributes"]; ok {
				for _, root := range []string{"email", "firstName", "lastName"} {
					if _, named := fields[root]; !named {
						delete(f.user, root)
					}
				}
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (f *skyNumberKeycloak) state() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return maps.Clone(f.user)
}

func fullUser(id uuid.UUID, attributes map[string][]string) map[string]any {
	return map[string]any{
		"id": id.String(), "username": "ada", "enabled": true, "emailVerified": true,
		"email": "ada.lovelace@std.yildiz.edu.tr", "firstName": "Ada", "lastName": "Lovelace",
		"requiredActions": []string{"CONFIGURE_TOTP"}, "createdTimestamp": 1758000000000,
		"attributes": attributes,
	}
}

func bareUser(id uuid.UUID) map[string]any {
	return map[string]any{"id": id.String(), "username": "bare", "enabled": true}
}

// withoutAttributes is the user's JSON without its attribute map.
func withoutAttributes(t *testing.T, user map[string]any) string {
	t.Helper()
	rest := maps.Clone(user)
	delete(rest, "attributes")
	encoded, err := json.Marshal(rest)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// The write sends the attribute map whole (Keycloak replaces it) with every
// attribute the person had, multi-valued ones in their order, plus skyNumber;
// and the username, e-mail and names as read, because Keycloak clears the
// e-mail and names a body with an attribute map leaves out. It never sends
// enabled, e-mail verification or required actions, and Keycloak's copy of
// the person keeps every field but skyNumber.
func TestKeycloakWriteSkyNumberSendsNoEnabledAndKeepsEveryOtherField(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		user   func(uuid.UUID) map[string]any
		before map[string][]string
		fields []string
	}{
		{name: "new sky number", user: func(id uuid.UUID) map[string]any {
			return fullUser(id, map[string][]string{
				"schoolEmail":    {"ada.lovelace@std.yildiz.edu.tr"},
				"personalEmail":  {"ada@example.com"},
				"department":     {"Bilgisayar Mühendisliği"},
				"university":     {"Yıldız Teknik Üniversitesi"},
				"legacyAliases":  {"zeta", "alpha", "mu"},
				"emptyOnPurpose": {},
			})
		}, fields: []string{"attributes", "email", "firstName", "lastName", "username"}},
		{name: "replaced sky number", user: func(id uuid.UUID) map[string]any {
			return fullUser(id, map[string][]string{
				"skyNumber":   {"SKY-0000001", "SKY-0000002"},
				"schoolEmail": {"ada.lovelace@std.yildiz.edu.tr"},
			})
		}, fields: []string{"attributes", "email", "firstName", "lastName", "username"}},
		{name: "no attributes yet", user: func(id uuid.UUID) map[string]any { return fullUser(id, nil) },
			fields: []string{"attributes", "email", "firstName", "lastName", "username"}},
		{name: "no e-mail or names", user: bareUser, fields: []string{"attributes", "username"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			id := uuid.New()
			before := tc.user(id)
			fake := &skyNumberKeycloak{id: id, user: maps.Clone(before)}
			directory := newAddressDirectory(fake.serve(t).URL)

			if err := directory.WriteSkyNumber(context.Background(), id, "SKY-0000042"); err != nil {
				t.Fatal(err)
			}
			if want := []string{http.MethodGet, http.MethodPut}; !reflect.DeepEqual(fake.methods, want) {
				t.Fatalf("methods = %v, want %v", fake.methods, want)
			}
			var body map[string]json.RawMessage
			if err := json.Unmarshal([]byte(fake.putBodies[0]), &body); err != nil {
				t.Fatal(err)
			}
			if keys := slices.Sorted(maps.Keys(body)); !reflect.DeepEqual(keys, tc.fields) {
				t.Fatalf("PUT body fields = %v, want %v: %s", keys, tc.fields, fake.putBodies[0])
			}
			var sent map[string][]string
			if err := json.Unmarshal(body["attributes"], &sent); err != nil {
				t.Fatal(err)
			}
			attributes, _ := before["attributes"].(map[string][]string)
			want := maps.Clone(attributes)
			if want == nil {
				want = map[string][]string{}
			}
			want["skyNumber"] = []string{"SKY-0000042"}
			if !reflect.DeepEqual(sent, want) {
				t.Fatalf("sent attributes = %v, want %v", sent, want)
			}
			after := fake.state()
			if got, want := withoutAttributes(t, after), withoutAttributes(t, before); got != want {
				t.Fatalf("user after the write = %s, want %s", got, want)
			}
		})
	}
}

// An erasure's disable can land between WriteSkyNumber's read (enabled) and
// its write. The write must not put the enabled=true it read back.
func TestKeycloakWriteSkyNumberDoesNotReEnableAUserDisabledBetweenReadAndWrite(t *testing.T) {
	t.Parallel()

	id := uuid.New()
	fake := &skyNumberKeycloak{id: id, user: fullUser(id, map[string][]string{"schoolEmail": {"ada.lovelace@std.yildiz.edu.tr"}})}
	directory := newAddressDirectory(fake.serve(t).URL)
	fake.afterGet = func() {
		if err := identity.NewAccountIdentity(directory).EnsureDisabled(context.Background(), id); err != nil {
			t.Errorf("interleaved disable: %v", err)
		}
	}

	if err := directory.WriteSkyNumber(context.Background(), id, "SKY-0000042"); err != nil {
		t.Fatal(err)
	}
	if want := []string{http.MethodGet, http.MethodPut, http.MethodPut}; !reflect.DeepEqual(fake.methods, want) {
		t.Fatalf("methods = %v, want the read, the interleaved disable and the write", fake.methods)
	}
	if want := `{"enabled":false}`; fake.putBodies[0] != want {
		t.Fatalf("interleaved body = %s, want %s", fake.putBodies[0], want)
	}
	got := fake.state()
	if got["enabled"] != false {
		t.Fatalf("enabled = %v after the write, want the interleaved disable kept", got["enabled"])
	}
	attributes, _ := got["attributes"].(map[string]any)
	if sky, _ := attributes["skyNumber"].([]any); len(sky) != 1 || sky[0] != "SKY-0000042" {
		t.Fatalf("attributes = %v, want the sky number written", attributes)
	}
}

func TestKeycloakWriteSkyNumberErrorsNameNoSubject(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		getStatus int
		putStatus int
		want      error
		status    string
	}{
		{name: "unknown user", getStatus: http.StatusNotFound, want: identity.ErrNotFound},
		{name: "deleted before the write", putStatus: http.StatusNotFound, want: identity.ErrNotFound},
		{name: "conflict", putStatus: http.StatusConflict, want: identity.ErrInvalid},
		{name: "rejected", putStatus: http.StatusBadRequest, status: "400"},
		{name: "server error", putStatus: http.StatusInternalServerError, status: "500"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			id := uuid.New()
			fake := &skyNumberKeycloak{id: id, user: fullUser(id, nil), getStatus: tc.getStatus, putStatus: tc.putStatus}
			err := newAddressDirectory(fake.serve(t).URL).WriteSkyNumber(context.Background(), id, "SKY-0000042")
			if tc.want != nil {
				if !errors.Is(err, tc.want) {
					t.Fatalf("error = %v, want %v", err, tc.want)
				}
				return
			}
			if err == nil {
				t.Fatal("expected an error")
			}
			if msg := err.Error(); !strings.Contains(msg, tc.status) || strings.Contains(msg, id.String()) || strings.Contains(msg, "kisi@example.com") {
				t.Fatalf("error = %q, want the status and neither the subject nor an address", msg)
			}
		})
	}
}
