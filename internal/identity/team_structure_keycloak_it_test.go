package identity

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The Keycloak tümleşik test of ADR 0065 tickets 05 and 07: core's
// Keycloak adapter, with a service account holding the realm-management
// roles production gives core, against a real Keycloak. It runs only when
// CORE_KEYCLOAK_IT_URL names a disposable Keycloak (start-dev is enough)
// whose master realm admin is CORE_KEYCLOAK_IT_ADMIN /
// CORE_KEYCLOAK_IT_ADMIN_PASSWORD; it creates a realm of its own and
// deletes it. Never point it at a shared Keycloak.
func TestTeamStructureAgainstKeycloak(t *testing.T) {
	base := strings.TrimRight(os.Getenv("CORE_KEYCLOAK_IT_URL"), "/")
	if base == "" {
		t.Skip("CORE_KEYCLOAK_IT_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	it := &keycloakIT{t: t, base: base, realm: "coreit" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")}
	it.admin = it.passwordToken(ctx, "master", "admin-cli", "", os.Getenv("CORE_KEYCLOAK_IT_ADMIN"), os.Getenv("CORE_KEYCLOAK_IT_ADMIN_PASSWORD"))
	it.createRealm(ctx)
	defer it.call(context.Background(), http.MethodDelete, "/admin/realms/"+it.realm, nil, nil)

	dir := NewKeycloak(KeycloakConfig{URL: base, Realm: it.realm, ClientID: "core", ClientSecret: "core-it-secret"})
	id := func(path string) string {
		g, err := dir.GetGroup(ctx, path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		return g.ID
	}
	weblab, front, mobilab, arge := id("/UYELER/ARGE/WEBLAB"), id("/UYELER/ARGE/WEBLAB/FRONT"), id("/UYELER/ARGE/MOBILAB"), id("/UYELER/ARGE")
	frontLeaders := id("/UYELER/ARGE/WEBLAB/FRONT/LIDERLER")

	// 05: a Sub-team with its display names in one write.
	temp, err := dir.CreateSubgroup(ctx, weblab, "TEMP", map[string]string{"display_name_tr": "Geçici"})
	if err != nil || temp.Path != "/UYELER/ARGE/WEBLAB/TEMP" || temp.Attributes["display_name_tr"] != "Geçici" {
		t.Fatalf("CreateSubgroup: %+v %v", temp, err)
	}
	sub, err := dir.CreateSubgroup(ctx, temp.ID, "TEMP-SUB", nil)
	if err != nil {
		t.Fatal(err)
	}
	leaders, err := dir.CreateSubgroup(ctx, temp.ID, "LIDERLER", nil)
	if err != nil {
		t.Fatal(err)
	}
	// 05: deleting a Team deletes every Group under it.
	if err := dir.DeleteGroup(ctx, temp.ID); err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{temp.ID, sub.ID, leaders.ID} {
		if _, _, err := dir.GroupNode(ctx, gone); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s survived the Team's deletion: %v", gone, err)
		}
	}

	// 07: the token before the move names the old path.
	before := it.groupsClaim(ctx, "")
	if !slices.Contains(before.groups, "/UYELER/ARGE/WEBLAB/FRONT") {
		t.Fatalf("groups before %v", before.groups)
	}
	moved, err := dir.MoveGroup(ctx, front, mobilab)
	if err != nil || moved.ID != front || moved.Path != "/UYELER/ARGE/MOBILAB/FRONT" {
		t.Fatalf("MoveGroup: %+v %v", moved, err)
	}
	node, parent, err := dir.GroupNode(ctx, front)
	if err != nil || parent != mobilab || node.Path != "/UYELER/ARGE/MOBILAB/FRONT" {
		t.Fatalf("after the move: %+v parent %s %v", node, parent, err)
	}
	l, lp, err := dir.GroupNode(ctx, frontLeaders)
	if err != nil || lp != front || l.Path != "/UYELER/ARGE/MOBILAB/FRONT/LIDERLER" {
		t.Fatalf("the Leader group after the move: %+v parent %s %v", l, lp, err)
	}
	members, err := dir.Members(ctx, front)
	if err != nil || len(members) != 1 {
		t.Fatalf("members after the move: %v %v", members, err)
	}
	// The refreshed token names the new path.
	after := it.groupsClaim(ctx, before.refresh)
	if !slices.Contains(after.groups, "/UYELER/ARGE/MOBILAB/FRONT") || slices.Contains(after.groups, "/UYELER/ARGE/WEBLAB/FRONT") {
		t.Fatalf("groups after the refresh %v", after.groups)
	}
	// To the realm's top and back under an area.
	if g, err := dir.MoveGroup(ctx, mobilab, ""); err != nil || g.Path != "/MOBILAB" {
		t.Fatalf("to the top: %+v %v", g, err)
	}
	if g, err := dir.MoveGroup(ctx, mobilab, arge); err != nil || g.Path != "/UYELER/ARGE/MOBILAB" {
		t.Fatalf("back: %+v %v", g, err)
	}
	if g, err := dir.GetGroup(ctx, front); err != nil || g.Path != "/UYELER/ARGE/MOBILAB/FRONT" {
		t.Fatalf("FRONT after its parent moved twice: %+v %v", g, err)
	}
	t.Logf("Keycloak %s: create with attributes, subtree delete, move (parentId, Leader group, members, refreshed token), move to the top and back: ok", it.version(ctx))
}

type keycloakIT struct {
	t     *testing.T
	base  string
	realm string
	admin string
}

type claimed struct {
	groups  []string
	refresh string
}

func (it *keycloakIT) passwordToken(ctx context.Context, realm, client, secret, user, password string) string {
	it.t.Helper()
	form := url.Values{"grant_type": {"password"}, "client_id": {client}, "username": {user}, "password": {password}}
	if secret != "" {
		form.Set("client_secret", secret)
	}
	var out struct {
		AccessToken string `json:"access_token"`
	}
	it.form(ctx, "/realms/"+realm+"/protocol/openid-connect/token", form, &out)
	return out.AccessToken
}

// groupsClaim is the web client's token for ada: by her password, or by
// the refresh token when given.
func (it *keycloakIT) groupsClaim(ctx context.Context, refresh string) claimed {
	it.t.Helper()
	form := url.Values{"client_id": {"web"}}
	if refresh == "" {
		form.Set("grant_type", "password")
		form.Set("username", "ada")
		form.Set("password", "ada-it-pass")
		form.Set("scope", "openid")
	} else {
		form.Set("grant_type", "refresh_token")
		form.Set("refresh_token", refresh)
	}
	var out struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	it.form(ctx, "/realms/"+it.realm+"/protocol/openid-connect/token", form, &out)
	parts := strings.Split(out.AccessToken, ".")
	if len(parts) != 3 {
		it.t.Fatal("no access token")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		it.t.Fatal(err)
	}
	var claims struct {
		Groups []string `json:"groups"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		it.t.Fatal(err)
	}
	return claimed{groups: claims.Groups, refresh: out.RefreshToken}
}

func (it *keycloakIT) form(ctx context.Context, path string, form url.Values, out any) {
	it.t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, it.base+path, strings.NewReader(form.Encode()))
	if err != nil {
		it.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	it.do(req, out)
}

func (it *keycloakIT) call(ctx context.Context, method, path string, body, out any) {
	it.t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			it.t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, it.base+path, reader)
	if err != nil {
		it.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+it.admin)
	req.Header.Set("Content-Type", "application/json")
	it.do(req, out)
}

func (it *keycloakIT) do(req *http.Request, out any) {
	it.t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		it.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		it.t.Fatalf("%s %s: %d %s", req.Method, req.URL.Path, resp.StatusCode, raw)
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			it.t.Fatalf("%s %s: %v", req.Method, req.URL.Path, err)
		}
	}
}

func (it *keycloakIT) version(ctx context.Context) string {
	var info struct {
		SystemInfo struct {
			Version string `json:"version"`
		} `json:"systemInfo"`
	}
	it.call(ctx, http.MethodGet, "/admin/serverinfo", nil, &info)
	return info.SystemInfo.Version
}

// createRealm makes the realm: core's confidential client with a service
// account holding production's realm-management roles for core, a public
// web client whose tokens carry full group paths, ada in FRONT and the
// Team tree.
func (it *keycloakIT) createRealm(ctx context.Context) {
	it.t.Helper()
	group := func(name string, subs ...map[string]any) map[string]any {
		return map[string]any{"name": name, "subGroups": subs}
	}
	it.call(ctx, http.MethodPost, "/admin/realms", map[string]any{
		"realm": it.realm, "enabled": true,
		"clients": []map[string]any{
			{"clientId": "core", "secret": "core-it-secret", "publicClient": false, "serviceAccountsEnabled": true, "standardFlowEnabled": false},
			{"clientId": "web", "publicClient": true, "directAccessGrantsEnabled": true, "standardFlowEnabled": false,
				"protocolMappers": []map[string]any{{
					"name": "groups", "protocol": "openid-connect", "protocolMapper": "oidc-group-membership-mapper",
					"config": map[string]string{"full.path": "true", "claim.name": "groups", "access.token.claim": "true", "id.token.claim": "false", "userinfo.token.claim": "false"},
				}}},
		},
		"groups": []map[string]any{group("UYELER", group("ARGE",
			group("WEBLAB", group("FRONT", group("LIDERLER"))),
			group("MOBILAB", group("LIDERLER")),
		))},
		"users": []map[string]any{{
			"username": "ada", "enabled": true, "email": "ada@example.test", "emailVerified": true, "firstName": "Ada", "lastName": "Lovelace",
			"credentials": []map[string]any{{"type": "password", "value": "ada-it-pass", "temporary": false}},
			"groups":      []string{"/UYELER/ARGE/WEBLAB/FRONT"},
		}},
	}, nil)
	var clients []struct {
		ID       string `json:"id"`
		ClientID string `json:"clientId"`
	}
	it.call(ctx, http.MethodGet, "/admin/realms/"+it.realm+"/clients", nil, &clients)
	var core, management string
	for _, c := range clients {
		switch c.ClientID {
		case "core":
			core = c.ID
		case "realm-management":
			management = c.ID
		}
	}
	var account struct {
		ID string `json:"id"`
	}
	it.call(ctx, http.MethodGet, "/admin/realms/"+it.realm+"/clients/"+core+"/service-account-user", nil, &account)
	var roles []map[string]any
	it.call(ctx, http.MethodGet, "/admin/realms/"+it.realm+"/clients/"+management+"/roles", nil, &roles)
	var grant []map[string]any
	for _, r := range roles {
		if slices.Contains([]string{"manage-users", "view-users", "query-users", "query-groups", "view-clients", "query-clients"}, fmt.Sprint(r["name"])) {
			grant = append(grant, r)
		}
	}
	if len(grant) != 6 {
		it.t.Fatalf("realm-management roles %d", len(grant))
	}
	it.call(ctx, http.MethodPost, "/admin/realms/"+it.realm+"/users/"+account.ID+"/role-mappings/clients/"+management, grant, nil)
}
