package identity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Nerzal/gocloak/v13"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/user"
	"golang.org/x/sync/singleflight"
)

type KeycloakConfig struct {
	// URL is Keycloak's public base (KEYCLOAK_URL), optionally with
	// /realms/<realm>. The token issuer core checks is derived from it, never
	// from AdminURL.
	URL   string
	Realm string
	// AdminURL (KEYCLOAK_ADMIN_URL, optional) is the base the Admin REST
	// calls and the service account's token request for them go to instead
	// of URL, e.g. Keycloak's address inside the Docker network. Keycloak
	// names its public address in iss whichever address issued the token.
	// Empty means URL.
	AdminURL     string
	ClientID     string
	ClientSecret string
	// Timeout bounds each request to Keycloak, token requests included; zero
	// means DefaultKeycloakTimeout.
	Timeout time.Duration
}

// DefaultKeycloakTimeout bounds each request to Keycloak.
const DefaultKeycloakTimeout = 30 * time.Second

type Keycloak struct {
	gc *gocloak.GoCloak
	// adminBase is where Admin REST (/admin/realms/<realm>/…) and the
	// service account's token request go: KeycloakConfig.AdminURL, or the
	// base of KeycloakConfig.URL when that is empty.
	adminBase    string
	realm        string
	clientID     string
	clientSecret string
	timeout      time.Duration
	// login asks Keycloak for the service account's token.
	login func(ctx context.Context) (*gocloak.JWT, error)
	// http is for the admin calls made without gocloak; gc has its own
	// client. Both end a request at timeout.
	http *http.Client

	// mu guards the fields below and is never held across a request to
	// Keycloak.
	mu          sync.Mutex
	token       string
	tokenExpiry time.Time
	clientsByID map[string]string
	// clientIDsByUUID are client ids by their Keycloak UUID (a composite
	// role's child names its client by UUID).
	clientIDsByUUID map[string]string

	// tokenFlight lets the callers that find the token expired share one
	// request for the next.
	tokenFlight singleflight.Group
}

func NewKeycloak(cfg KeycloakConfig) *Keycloak {
	base := strings.TrimRight(cfg.URL, "/")
	realm := cfg.Realm
	if parts := strings.SplitN(base, "/realms/", 2); len(parts) == 2 {
		base = parts[0]
		if realm == "" {
			realm = parts[1]
		}
	}
	if admin := keycloakBase(cfg.AdminURL); admin != "" {
		base = admin
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultKeycloakTimeout
	}
	// gocloak sends the token request and its own Admin REST calls to base.
	gc := gocloak.NewClient(base)
	// resty's client, which gocloak sends every request through, has no
	// timeout of its own.
	gc.RestyClient().SetTimeout(timeout)
	k := &Keycloak{
		timeout:         timeout,
		http:            &http.Client{Timeout: timeout},
		gc:              gc,
		adminBase:       base,
		realm:           realm,
		clientID:        cfg.ClientID,
		clientSecret:    cfg.ClientSecret,
		clientsByID:     map[string]string{},
		clientIDsByUUID: map[string]string{},
	}
	k.login = func(ctx context.Context) (*gocloak.JWT, error) {
		return k.gc.LoginClient(ctx, k.clientID, k.clientSecret, k.realm)
	}
	return k
}

// keycloakBase is a Keycloak base URL without surrounding blanks, trailing
// slashes or a /realms/<realm> suffix.
func keycloakBase(raw string) string {
	base := strings.TrimRight(strings.TrimSpace(raw), "/")
	if before, _, found := strings.Cut(base, "/realms/"); found {
		base = strings.TrimRight(before, "/")
	}
	return base
}

// ParseKeycloakAdminURL checks KEYCLOAK_ADMIN_URL and answers the base core
// sends Admin REST to: "" when it is unset or blank, otherwise an absolute
// http(s) URL without trailing slashes or a /realms/<realm> suffix (the
// shape KEYCLOAK_URL accepts), e.g. http://keycloak:8080. A context path is
// kept. The errors name the variable, never its value.
func ParseKeycloakAdminURL(raw string) (string, error) {
	base := keycloakBase(raw)
	if base == "" {
		return "", nil
	}
	u, err := url.Parse(base)
	if err != nil {
		return "", errors.New("KEYCLOAK_ADMIN_URL is not a URL")
	}
	if scheme := strings.ToLower(u.Scheme); (scheme != "http" && scheme != "https") || u.Host == "" || u.Opaque != "" {
		return "", errors.New("KEYCLOAK_ADMIN_URL must be an absolute http or https URL, e.g. http://keycloak:8080")
	}
	if u.User != nil {
		return "", errors.New("KEYCLOAK_ADMIN_URL must not carry credentials")
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", errors.New("KEYCLOAK_ADMIN_URL must not have a query or a fragment")
	}
	for _, segment := range strings.Split(u.Path, "/") {
		if segment == "admin" || segment == "realms" {
			return "", errors.New("KEYCLOAK_ADMIN_URL is Keycloak's base URL; core adds /admin/realms/<realm> itself")
		}
	}
	return base, nil
}

// accessToken answers the service account's token, asking Keycloak for a new
// one when the held one is about to expire. The request runs outside k.mu, so
// a Keycloak that stops answering holds up only the callers that need the new
// token, and each of those leaves when its own ctx ends.
func (k *Keycloak) accessToken(ctx context.Context) (string, error) {
	if token, ok := k.cachedToken(); ok {
		return token, nil
	}
	flight := k.tokenFlight.DoChan("token", k.refreshToken)
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case result := <-flight:
		if result.Err != nil {
			return "", result.Err
		}
		return result.Val.(string), nil
	}
}

func (k *Keycloak) cachedToken() (string, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.token != "" && time.Now().Before(k.tokenExpiry) {
		return k.token, true
	}
	return "", false
}

// refreshToken is what the callers waiting for a token share. It belongs to
// none of them: it must not end because the caller that started it gave up,
// and it must not keep that caller's request (which fasthttp reuses) alive.
// It ends at the timeout.
func (k *Keycloak) refreshToken() (token any, err error) {
	// singleflight runs this in a goroutine of its own and, should it panic,
	// panics again there, which ends the whole process. The callers get an
	// error instead.
	defer func() {
		if r := recover(); r != nil {
			token, err = "", fmt.Errorf("identity: keycloak token request panicked: %v", r)
		}
	}()
	// A refresh may have finished between the caller's check and this one.
	if cached, ok := k.cachedToken(); ok {
		return cached, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), k.timeout)
	defer cancel()
	// Keycloak dates the token when it issues it, so its life counts from
	// before the request, not from when the answer arrived.
	start := time.Now()
	jwt, err := k.login(ctx)
	if err != nil {
		return "", err
	}
	ttl := time.Duration(jwt.ExpiresIn-30) * time.Second
	if ttl < time.Second {
		ttl = 30 * time.Second
	}
	k.mu.Lock()
	k.token = jwt.AccessToken
	k.tokenExpiry = start.Add(ttl)
	k.mu.Unlock()
	return jwt.AccessToken, nil
}

func mapKCErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrInvalid) {
		return err
	}
	var api *gocloak.APIError
	if errors.As(err, &api) && api != nil {
		if api.Code == 404 {
			return ErrNotFound
		}
		if api.Code == 409 {
			return ErrInvalid
		}
	}
	var val gocloak.APIError
	if errors.As(err, &val) {
		if val.Code == 404 {
			return ErrNotFound
		}
		if val.Code == 409 {
			return ErrInvalid
		}
	}
	return err
}

func (k *Keycloak) ListGroups(ctx context.Context) ([]Group, error) {
	token, err := k.accessToken(ctx)
	if err != nil {
		return nil, err
	}
	tops, err := k.gc.GetGroups(ctx, token, k.realm, gocloak.GetGroupsParams{
		Full: gocloak.BoolP(true),
		Max:  gocloak.IntP(1000),
	})
	if err != nil {
		return nil, mapKCErr(err)
	}
	out, err := k.collectGroups(ctx, token, tops)
	if err != nil {
		return nil, err
	}
	return dedupGroups(out), nil
}

func (k *Keycloak) collectGroups(ctx context.Context, token string, gs []*gocloak.Group) ([]Group, error) {
	out := flattenGroups(gs)
	for _, g := range gs {
		if g == nil || g.ID == nil {
			continue
		}
		kids, err := k.children(ctx, token, *g.ID)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				continue
			}
			return nil, err
		}
		nested, err := k.collectGroups(ctx, token, kids)
		if err != nil {
			return nil, err
		}
		out = append(out, nested...)
	}
	return out, nil
}

func (k *Keycloak) children(ctx context.Context, token, groupID string) ([]*gocloak.Group, error) {
	out := make([]*gocloak.Group, 0)
	first := 0
	const pageSize = 100
	for {
		var page []*gocloak.Group
		resp, err := k.gc.GetRequestWithBearerAuth(ctx, token).
			SetResult(&page).
			SetQueryParams(map[string]string{
				"first":               strconv.Itoa(first),
				"max":                 strconv.Itoa(pageSize),
				"briefRepresentation": "false",
			}).
			Get(k.adminBase + "/admin/realms/" + k.realm + "/groups/" + groupID + "/children")
		if err != nil {
			return nil, err
		}
		if resp.IsError() {
			if resp.StatusCode() == 404 {
				return nil, ErrNotFound
			}
			return nil, errors.New("identity: keycloak children failed")
		}
		out = append(out, page...)
		if len(page) < pageSize {
			return out, nil
		}
		first += len(page)
	}
}

func (k *Keycloak) GetGroup(ctx context.Context, idOrPath string) (Group, error) {
	token, err := k.accessToken(ctx)
	if err != nil {
		return Group{}, err
	}
	g, err := k.fetchGroup(ctx, token, idOrPath)
	if err != nil {
		return Group{}, mapKCErr(err)
	}
	return groupFrom(g), nil
}

func (k *Keycloak) fetchGroup(ctx context.Context, token, idOrPath string) (*gocloak.Group, error) {
	if _, err := uuid.Parse(idOrPath); err == nil {
		g, err := k.gc.GetGroup(ctx, token, k.realm, idOrPath)
		if err != nil {
			return nil, err
		}
		return g, nil
	}
	path := idOrPath
	if strings.Contains(path, "/") {
		g, err := k.gc.GetGroupByPath(ctx, token, k.realm, strings.TrimPrefix(path, "/"))
		if err == nil {
			return g, nil
		}
		if mapKCErr(err) != ErrNotFound {
			return nil, err
		}
	}
	found, err := k.searchGroup(ctx, token, idOrPath)
	if err != nil {
		return nil, err
	}
	return found, nil
}

func (k *Keycloak) searchGroup(ctx context.Context, token, nameOrPath string) (*gocloak.Group, error) {
	needle := strings.TrimPrefix(nameOrPath, "/")
	gs, err := k.gc.GetGroups(ctx, token, k.realm, gocloak.GetGroupsParams{
		Full:   gocloak.BoolP(true),
		Search: &needle,
		Max:    gocloak.IntP(1000),
	})
	if err != nil {
		return nil, err
	}
	flat := flattenGroups(gs)
	for i := range flat {
		if flat[i].ID == nameOrPath || flat[i].Path == nameOrPath || flat[i].Name == nameOrPath || strings.TrimPrefix(flat[i].Path, "/") == needle {
			g, err := k.gc.GetGroup(ctx, token, k.realm, flat[i].ID)
			if err != nil {
				return nil, err
			}
			return g, nil
		}
	}
	return nil, ErrNotFound
}

func (k *Keycloak) CreateGroup(ctx context.Context, parentRef, name string) (Group, error) {
	token, err := k.accessToken(ctx)
	if err != nil {
		return Group{}, err
	}
	payload := gocloak.Group{Name: &name}
	var id string
	if parentRef == "" {
		id, err = k.gc.CreateGroup(ctx, token, k.realm, payload)
	} else {
		parent, ferr := k.fetchGroup(ctx, token, parentRef)
		if ferr != nil {
			return Group{}, mapKCErr(ferr)
		}
		id, err = k.gc.CreateChildGroup(ctx, token, k.realm, *parent.ID, payload)
	}
	if err != nil {
		return Group{}, mapKCErr(err)
	}
	fresh, err := k.gc.GetGroup(ctx, token, k.realm, id)
	if err != nil {
		return Group{}, mapKCErr(err)
	}
	return groupFrom(fresh), nil
}

func (k *Keycloak) UpdateGroup(ctx context.Context, g Group) (Group, error) {
	token, err := k.accessToken(ctx)
	if err != nil {
		return Group{}, err
	}
	existing, err := k.fetchGroup(ctx, token, g.ID)
	if err != nil && g.Path != "" {
		existing, err = k.fetchGroup(ctx, token, g.Path)
	}
	if err != nil {
		return Group{}, mapKCErr(err)
	}
	name := gocloak.PString(existing.Name)
	if g.Name != "" {
		name = g.Name
	}
	updated := gocloak.Group{
		ID:         existing.ID,
		Name:       &name,
		Path:       existing.Path,
		Attributes: attrsToKC(g.Attributes),
	}
	if err := k.gc.UpdateGroup(ctx, token, k.realm, updated); err != nil {
		return Group{}, mapKCErr(err)
	}
	fresh, err := k.gc.GetGroup(ctx, token, k.realm, *existing.ID)
	if err != nil {
		return Group{}, mapKCErr(err)
	}
	return groupFrom(fresh), nil
}

// DeleteGroup uses an exact id: destructive writes never resolve a name or path.
func (k *Keycloak) DeleteGroup(ctx context.Context, groupID string) error {
	token, err := k.accessToken(ctx)
	if err != nil {
		return err
	}
	return mapKCErr(k.gc.DeleteGroup(ctx, token, k.realm, groupID))
}

func (k *Keycloak) Subgroups(ctx context.Context, groupID string) ([]Group, error) {
	token, err := k.accessToken(ctx)
	if err != nil {
		return nil, err
	}
	parent, err := k.fetchGroup(ctx, token, groupID)
	if err != nil {
		return nil, mapKCErr(err)
	}
	kids, err := k.children(ctx, token, *parent.ID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return []Group{}, nil
		}
		return nil, err
	}
	out := make([]Group, 0, len(kids))
	for _, child := range kids {
		out = append(out, groupFrom(child))
	}
	return out, nil
}

func (k *Keycloak) Members(ctx context.Context, groupID string) ([]Person, error) {
	token, err := k.accessToken(ctx)
	if err != nil {
		return nil, err
	}
	g, err := k.fetchGroup(ctx, token, groupID)
	if err != nil {
		return nil, mapKCErr(err)
	}
	users, err := k.gc.GetGroupMembers(ctx, token, k.realm, *g.ID, gocloak.GetGroupsParams{Max: gocloak.IntP(10000)})
	if err != nil {
		return nil, mapKCErr(err)
	}
	out := make([]Person, 0, len(users))
	for _, u := range users {
		p, err := personFrom(u)
		if err != nil {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

func (k *Keycloak) AddMember(ctx context.Context, groupID string, userID uuid.UUID) error {
	token, err := k.accessToken(ctx)
	if err != nil {
		return err
	}
	g, err := k.fetchGroup(ctx, token, groupID)
	if err != nil {
		return mapKCErr(err)
	}
	if _, err := k.gc.GetUserByID(ctx, token, k.realm, userID.String()); err != nil {
		return mapKCErr(err)
	}
	return mapKCErr(k.gc.AddUserToGroup(ctx, token, k.realm, userID.String(), *g.ID))
}

func (k *Keycloak) RemoveMember(ctx context.Context, groupID string, userID uuid.UUID) error {
	token, err := k.accessToken(ctx)
	if err != nil {
		return err
	}
	g, err := k.fetchGroup(ctx, token, groupID)
	if err != nil {
		return mapKCErr(err)
	}
	return mapKCErr(k.gc.DeleteUserFromGroup(ctx, token, k.realm, userID.String(), *g.ID))
}

func (k *Keycloak) ListUsers(ctx context.Context) ([]Person, error) {
	token, err := k.accessToken(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Person, 0)
	first := 0
	const pageSize = 20
	for {
		u := k.adminBase + "/admin/realms/" + k.realm + "/users?first=" + strconv.Itoa(first) + "&max=" + strconv.Itoa(pageSize) + "&briefRepresentation=true"
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := k.http.Do(req)
		if err != nil {
			return nil, err
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		if resp.StatusCode >= 400 {
			return nil, mapKCErr(&gocloak.APIError{Code: resp.StatusCode, Message: string(body)})
		}
		var chunks []json.RawMessage
		if err := json.Unmarshal(body, &chunks); err != nil {
			return nil, err
		}
		for _, chunk := range chunks {
			var row struct {
				ID        string `json:"id"`
				Email     string `json:"email"`
				FirstName string `json:"firstName"`
				LastName  string `json:"lastName"`
				Username  string `json:"username"`
				Enabled   bool   `json:"enabled"`
			}
			if err := json.Unmarshal(chunk, &row); err != nil {
				continue
			}
			id, err := uuid.Parse(row.ID)
			if err != nil {
				continue
			}
			out = append(out, Person{
				ID: id, Email: row.Email, FirstName: row.FirstName,
				LastName: row.LastName, Username: row.Username, Enabled: row.Enabled,
			})
		}
		if len(chunks) < pageSize {
			return out, nil
		}
		first += len(chunks)
	}
}

// YTUAttributes reads the `university` and `department` attributes of every
// account that carries a university, for the one-time YTÜ profile backfill.
// It only reads (view-users is enough) and pages through the realm.
func (k *Keycloak) YTUAttributes(ctx context.Context) ([]user.YTUAttributes, error) {
	token, err := k.accessToken(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]user.YTUAttributes, 0)
	const pageSize = 100
	for first := 0; ; first += pageSize {
		u := k.adminBase + "/admin/realms/" + url.PathEscape(k.realm) + "/users?first=" + strconv.Itoa(first) +
			"&max=" + strconv.Itoa(pageSize) + "&briefRepresentation=false"
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := k.http.Do(req)
		if err != nil {
			return nil, err
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		if resp.StatusCode >= 400 {
			return nil, mapKCErr(&gocloak.APIError{Code: resp.StatusCode, Message: http.StatusText(resp.StatusCode)})
		}
		var rows []struct {
			ID         string              `json:"id"`
			Attributes map[string][]string `json:"attributes"`
		}
		if err := json.Unmarshal(body, &rows); err != nil {
			return nil, err
		}
		for _, row := range rows {
			id, err := uuid.Parse(row.ID)
			if err != nil {
				continue
			}
			attr := func(name string) string {
				for _, v := range row.Attributes[name] {
					if strings.TrimSpace(v) != "" {
						return v
					}
				}
				return ""
			}
			if university := attr("university"); university != "" {
				out = append(out, user.YTUAttributes{ID: id, University: university, Department: attr("department")})
			}
		}
		if len(rows) < pageSize {
			return out, nil
		}
	}
}

func (k *Keycloak) SearchUsers(ctx context.Context, query string, limit int) ([]Person, error) {
	token, err := k.accessToken(ctx)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 20
	}
	params := url.Values{}
	params.Set("first", "0")
	params.Set("max", strconv.Itoa(limit))
	params.Set("briefRepresentation", "true")
	params.Set("search", strings.TrimSpace(query))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, k.adminBase+"/admin/realms/"+k.realm+"/users?"+params.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := k.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, mapKCErr(&gocloak.APIError{Code: resp.StatusCode, Message: string(body)})
	}
	var chunks []json.RawMessage
	if err := json.Unmarshal(body, &chunks); err != nil {
		return nil, err
	}
	out := make([]Person, 0, len(chunks))
	for _, chunk := range chunks {
		var row struct {
			ID        string `json:"id"`
			Email     string `json:"email"`
			FirstName string `json:"firstName"`
			LastName  string `json:"lastName"`
			Username  string `json:"username"`
			Enabled   bool   `json:"enabled"`
		}
		if err := json.Unmarshal(chunk, &row); err != nil {
			continue
		}
		id, err := uuid.Parse(row.ID)
		if err != nil {
			continue
		}
		out = append(out, Person{
			ID: id, Email: row.Email, FirstName: row.FirstName,
			LastName: row.LastName, Username: row.Username, Enabled: row.Enabled,
		})
	}
	return out, nil
}

func (k *Keycloak) UsersWithClientRole(ctx context.Context, clientID, role string) ([]Person, error) {
	token, err := k.accessToken(ctx)
	if err != nil {
		return nil, err
	}
	clientUUID, err := k.clientUUID(ctx, token, clientID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return []Person{}, nil
		}
		return nil, err
	}
	roles := []string{role}
	if strings.HasPrefix(role, "skyforms:") && role != "skyforms:*" {
		roles = append(roles, "skyforms:*")
	}
	seen := map[uuid.UUID]struct{}{}
	out := make([]Person, 0)
	add := func(p Person) {
		if p.ID == uuid.Nil {
			return
		}
		if _, ok := seen[p.ID]; ok {
			return
		}
		seen[p.ID] = struct{}{}
		out = append(out, p)
	}
	for _, name := range roles {
		groups, users, err := k.clientRoleHolders(ctx, token, clientUUID, name)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				continue
			}
			return nil, err
		}
		for _, g := range groups {
			members, err := k.Members(ctx, g.ID)
			if err != nil {
				if errors.Is(err, ErrNotFound) {
					continue
				}
				return nil, err
			}
			for _, person := range members {
				add(person)
			}
		}
		for _, person := range users {
			add(person)
		}
	}
	return out, nil
}

func (k *Keycloak) clientRoleHolders(ctx context.Context, token, clientUUID, role string) ([]Group, []Person, error) {
	base := k.adminBase + "/admin/realms/" + k.realm + "/clients/" + clientUUID + "/roles/" + url.PathEscape(role)
	groups := make([]Group, 0)
	first := 0
	const pageSize = 100
	for {
		var page []*gocloak.Group
		resp, err := k.gc.GetRequestWithBearerAuth(ctx, token).SetResult(&page).SetQueryParams(map[string]string{"first": strconv.Itoa(first), "max": strconv.Itoa(pageSize), "briefRepresentation": "true"}).Get(base + "/groups")
		if err != nil {
			return nil, nil, err
		}
		if resp.IsError() {
			if resp.StatusCode() == 404 {
				return nil, nil, ErrNotFound
			}
			return nil, nil, errors.New("identity: keycloak role groups failed")
		}
		for _, g := range page {
			groups = append(groups, groupFrom(g))
		}
		if len(page) < pageSize {
			break
		}
		first += len(page)
	}
	users := make([]Person, 0)
	first = 0
	for {
		var page []*gocloak.User
		resp, err := k.gc.GetRequestWithBearerAuth(ctx, token).SetResult(&page).SetQueryParams(map[string]string{"first": strconv.Itoa(first), "max": strconv.Itoa(pageSize), "briefRepresentation": "true"}).Get(base + "/users")
		if err != nil {
			return nil, nil, err
		}
		if resp.IsError() {
			if resp.StatusCode() == 404 {
				return groups, users, nil
			}
			return nil, nil, errors.New("identity: keycloak role users failed")
		}
		for _, u := range page {
			person, err := personFrom(u)
			if err != nil {
				continue
			}
			users = append(users, person)
		}
		if len(page) < pageSize {
			return groups, users, nil
		}
		first += len(page)
	}
}

func (k *Keycloak) CreateUser(ctx context.Context, p Person) (Person, error) {
	token, err := k.accessToken(ctx)
	if err != nil {
		return Person{}, err
	}
	username := p.Username
	if username == "" {
		username = p.Email
	}
	enabled := true
	id, err := k.gc.CreateUser(ctx, token, k.realm, gocloak.User{
		Username:  &username,
		Email:     &p.Email,
		FirstName: &p.FirstName,
		LastName:  &p.LastName,
		Enabled:   &enabled,
	})
	if err != nil {
		return Person{}, mapKCErr(err)
	}
	u, err := k.gc.GetUserByID(ctx, token, k.realm, id)
	if err != nil {
		return Person{}, mapKCErr(err)
	}
	return personFrom(u)
}

func (k *Keycloak) GetUser(ctx context.Context, id uuid.UUID) (Person, error) {
	token, err := k.accessToken(ctx)
	if err != nil {
		return Person{}, err
	}
	u, err := k.gc.GetUserByID(ctx, token, k.realm, id.String())
	if err != nil {
		return Person{}, mapKCErr(err)
	}
	return personFrom(u)
}

// erasureAddressAttributes are the user attributes that hold an address of
// the person besides the Primary e-mail (`email`): the School e-mail and the
// Personal e-mail.
var erasureAddressAttributes = []string{"schoolEmail", "personalEmail"}

// UserAddresses reads, without changing anything, every address Keycloak holds
// for the person: the Primary e-mail and the School and Personal e-mail
// attributes, raw and without blanks. A user Keycloak does not know is
// ErrNotFound.
//
// Account erasure sends these addresses to the services (ADR-0051), so no
// error this returns names the subject or an address: the admin URL holds the
// subject and a response body may hold anything.
func (k *Keycloak) UserAddresses(ctx context.Context, id uuid.UUID) ([]string, error) {
	token, err := k.accessToken(ctx)
	if err != nil {
		return nil, errors.New("identity: user address lookup could not get an admin token")
	}
	u, err := k.gc.GetUserByID(ctx, token, k.realm, id.String())
	if err != nil {
		if mapKCErr(err) == ErrNotFound {
			return nil, ErrNotFound
		}
		var api *gocloak.APIError
		if errors.As(err, &api) && api != nil && api.Code != 0 {
			return nil, fmt.Errorf("identity: user address lookup failed with status %d", api.Code)
		}
		return nil, errors.New("identity: user address lookup failed")
	}
	var addresses []string
	if u.Email != nil && strings.TrimSpace(*u.Email) != "" {
		addresses = append(addresses, *u.Email)
	}
	if u.Attributes != nil {
		for _, name := range erasureAddressAttributes {
			for _, value := range (*u.Attributes)[name] {
				if strings.TrimSpace(value) != "" {
					addresses = append(addresses, value)
				}
			}
		}
	}
	return addresses, nil
}

// DisableUser sends Keycloak exactly `{"enabled":false}` and nothing else.
// Keycloak copies the request body into the UPDATE admin event, which is not
// deleted with the user, so a full representation would leave the person's
// e-mail, names and attributes there. Keycloak 26.7.4 leaves every field the
// body does not name unchanged (e-skylab-keycloak tests/erasure-event-pii.sh).
// A user Keycloak does not know is ErrNotFound; an already disabled user is
// disabled again. No error names the subject.
func (k *Keycloak) DisableUser(ctx context.Context, id uuid.UUID) error {
	token, err := k.accessToken(ctx)
	if err != nil {
		return err
	}
	resp, err := k.gc.GetRequestWithBearerAuth(ctx, token).
		SetBody(map[string]bool{"enabled": false}).
		Put(k.adminBase + "/admin/realms/" + url.PathEscape(k.realm) + "/users/" + id.String())
	if err != nil {
		return errors.New("identity: keycloak disable user request failed")
	}
	switch {
	case !resp.IsError():
		return nil
	case resp.StatusCode() == http.StatusNotFound:
		return ErrNotFound
	case resp.StatusCode() == http.StatusConflict:
		return ErrInvalid
	default:
		return fmt.Errorf("identity: keycloak disable user failed with status %d", resp.StatusCode())
	}
}

func (k *Keycloak) DeleteUser(ctx context.Context, id uuid.UUID) error {
	token, err := k.accessToken(ctx)
	if err != nil {
		return err
	}
	return mapKCErr(k.gc.DeleteUser(ctx, token, k.realm, id.String()))
}

// GroupsForUser reads the person's Groups from Keycloak Admin REST: their
// direct memberships, each with its full path, which is what the groups
// claim of their token carries (Keycloak's Group Membership mapper). It
// pages through all of them. A failure on any page is an error with no
// Groups, never a shorter list, so a person whose Groups could not be read
// never reads as a person with fewer. A user Keycloak does not know is
// ErrNotFound. No error names the person: their id is in the request
// address and Keycloak's error body may hold anything.
//
// Keycloak leaves out of each page the Groups the caller may not view, and
// a short page ends the list. Core's service account holds view-users
// (docs/keycloak-admin-permissions.md), which views every Group; without it
// the list could come back shorter.
func (k *Keycloak) GroupsForUser(ctx context.Context, userID uuid.UUID) ([]Group, error) {
	token, err := k.accessToken(ctx)
	if err != nil {
		return nil, err
	}
	const pageSize = 100
	out := make([]Group, 0)
	for first := 0; ; first += pageSize {
		var page []*gocloak.Group
		resp, err := k.gc.GetRequestWithBearerAuth(ctx, token).
			SetResult(&page).
			SetQueryParams(map[string]string{
				"first":               strconv.Itoa(first),
				"max":                 strconv.Itoa(pageSize),
				"briefRepresentation": "false",
			}).
			Get(k.adminBase + "/admin/realms/" + url.PathEscape(k.realm) + "/users/" + userID.String() + "/groups")
		if err != nil {
			// The transport error quotes the address; keep only its cause.
			var addressed *url.Error
			if errors.As(err, &addressed) {
				err = addressed.Err
			}
			return nil, fmt.Errorf("identity: keycloak user groups request failed: %w", err)
		}
		switch {
		case resp.StatusCode() == http.StatusNotFound:
			return nil, ErrNotFound
		case resp.IsError():
			return nil, fmt.Errorf("identity: keycloak user groups failed with status %d", resp.StatusCode())
		}
		// Only the direct memberships: a subgroup nested in the answer is
		// not one of the person's Groups.
		for _, g := range page {
			if g != nil {
				out = append(out, groupFrom(g))
			}
		}
		if len(page) < pageSize {
			return out, nil
		}
	}
}

// GroupNode reads the Group id (a Keycloak id; anything else is not looked
// up and is ErrNotFound) with the id of the Group above it, "" for a Group
// at the realm's top (GroupNodes). gocloak's Group has no parentId, so the
// representation is read here.
func (k *Keycloak) GroupNode(ctx context.Context, id string) (Group, string, error) {
	if _, err := uuid.Parse(id); err != nil {
		return Group{}, "", ErrNotFound
	}
	token, err := k.accessToken(ctx)
	if err != nil {
		return Group{}, "", err
	}
	var rep struct {
		gocloak.Group
		ParentID *string `json:"parentId,omitempty"`
	}
	resp, err := k.gc.GetRequestWithBearerAuth(ctx, token).
		SetResult(&rep).
		Get(k.adminBase + "/admin/realms/" + url.PathEscape(k.realm) + "/groups/" + url.PathEscape(id))
	if err != nil {
		var addressed *url.Error
		if errors.As(err, &addressed) {
			err = addressed.Err
		}
		return Group{}, "", fmt.Errorf("identity: keycloak group request failed: %w", err)
	}
	switch {
	case resp.StatusCode() == http.StatusNotFound:
		return Group{}, "", ErrNotFound
	case resp.IsError():
		return Group{}, "", fmt.Errorf("identity: keycloak group read failed with status %d", resp.StatusCode())
	}
	g := groupFrom(&rep.Group)
	if g.ID != id {
		return Group{}, "", fmt.Errorf("identity: keycloak answered another group")
	}
	parent := ""
	if rep.ParentID != nil {
		parent = *rep.ParentID
	}
	return g, parent, nil
}

// maxCompositeDepth bounds the walk into composite roles: a role nested
// deeper than this is taken as protected.
const maxCompositeDepth = 8

// GroupProtection reads whether the Group groupID's own role mappings make
// it a Protected group (GroupProtections): any realm role it holds in
// effect (GET /groups/{id}/role-mappings/realm/composite), or a client role
// that is no app role (authz.DelegableRole: an admin role or one nobody has
// classified), or an app role that holds, as a composite at any depth, a
// realm role or a client role that is no app role. Realm roles are not
// opened: any one counts (authz.ClassifyRealmRole; the Java era's realm
// roles leave in the role cleanup's F3), so core needs no view-realm.
// A client role's composites are read by id (GET /roles-by-id/{id}/
// composites, view-clients) and their clients by UUID (GET /clients/{id},
// kept for the process).
func (k *Keycloak) GroupProtection(ctx context.Context, groupID string) (Protection, error) {
	token, err := k.accessToken(ctx)
	if err != nil {
		return Protection{}, err
	}
	mapping, err := k.gc.GetRoleMappingByGroupID(ctx, token, k.realm, groupID)
	if err != nil {
		return Protection{}, mapKCErr(err)
	}
	// The realm roles are read on their own, in effect: Keycloak leaves them
	// out of /role-mappings, and out of a client role's composites, for a
	// caller without view-realm, which core's service account does not hold
	// (Keycloak 26.7.4, 2026-10-12). /role-mappings/realm/composite answers
	// every realm role the Group holds, through client composites too, so a
	// client composite that hides a realm role (client → realm →
	// realm-management) is seen here.
	realm, err := k.gc.GetCompositeRealmRolesByGroupID(ctx, token, k.realm, groupID)
	if err != nil {
		return Protection{}, mapKCErr(err)
	}
	for _, r := range realm {
		if r != nil {
			return Protection{Protected: true, Reason: "realm role " + gocloak.PString(r.Name)}, nil
		}
	}
	if mapping == nil {
		return Protection{}, nil
	}
	seen := map[string]bool{}
	var check func(r gocloak.Role, clientID string, depth int) (Protection, error)
	check = func(r gocloak.Role, clientID string, depth int) (Protection, error) {
		name := gocloak.PString(r.Name)
		if got := clientRoleProtection(clientID, name); got.Protected {
			return got, nil
		}
		if !gocloak.PBool(r.Composite) || r.ID == nil || seen[*r.ID] {
			return Protection{}, nil
		}
		seen[*r.ID] = true
		if depth >= maxCompositeDepth {
			return Protection{Protected: true, Reason: "composite " + clientID + ":" + name + " nested too deep"}, nil
		}
		children, err := k.gc.GetCompositeRolesByRoleID(ctx, token, k.realm, *r.ID)
		if err != nil {
			return Protection{}, mapKCErr(err)
		}
		for _, child := range children {
			if child == nil {
				continue
			}
			if !gocloak.PBool(child.ClientRole) {
				return Protection{Protected: true, Reason: "composite " + clientID + ":" + name + " holds realm role " + gocloak.PString(child.Name)}, nil
			}
			childClient, err := k.clientIDOf(ctx, token, gocloak.PString(child.ContainerID))
			if err != nil {
				return Protection{}, err
			}
			if got, err := check(*child, childClient, depth+1); err != nil || got.Protected {
				if got.Protected {
					got.Reason = "composite " + clientID + ":" + name + " holds " + got.Reason
				}
				return got, err
			}
		}
		return Protection{}, nil
	}
	for key, cm := range mapping.ClientMappings {
		if cm == nil || cm.Mappings == nil {
			continue
		}
		clientID := key
		if cm.Client != nil && *cm.Client != "" {
			clientID = *cm.Client
		}
		for _, r := range *cm.Mappings {
			if got, err := check(r, clientID, 0); err != nil || got.Protected {
				return got, err
			}
		}
	}
	return Protection{}, nil
}

// KeycloakAdminRole is a realm-management role the Group groupID holds in
// effect, "" for none (KeycloakAdminGroups): Keycloak's GET
// /groups/{id}/role-mappings/clients/{realm-management}/composite, which
// expands composite roles, a realm role's included. It needs what
// GroupProtection needs (view-clients for the client's id, the Group's role
// mappings); a realm without the client answers "".
func (k *Keycloak) KeycloakAdminRole(ctx context.Context, groupID string) (string, error) {
	token, err := k.accessToken(ctx)
	if err != nil {
		return "", err
	}
	client, err := k.clientUUID(ctx, token, KeycloakAdminClient)
	if errors.Is(err, ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	roles, err := k.gc.GetCompositeClientRolesByGroupID(ctx, token, k.realm, client, groupID)
	if err != nil {
		return "", mapKCErr(err)
	}
	for _, r := range roles {
		if r != nil && gocloak.PString(r.Name) != "" {
			return gocloak.PString(r.Name), nil
		}
	}
	return "", nil
}

// RoleHoldsKeycloakAdmin is the realm-management role role is or holds as
// a composite at any depth (maxCompositeDepth; deeper counts as one), ""
// for none (KeycloakAdminGroups). A realm role inside the composite answers
// that role: its own composites are not readable without view-realm, so
// it counts as one that may hold administration (fail-closed). The
// composites are read by id (GET /roles-by-id/{id}/composites) and their
// clients by UUID, as GroupProtection reads them.
func (k *Keycloak) RoleHoldsKeycloakAdmin(ctx context.Context, role ClientRole) (string, error) {
	if role.ClientID == KeycloakAdminClient {
		return role.Role, nil
	}
	token, err := k.accessToken(ctx)
	if err != nil {
		return "", err
	}
	r, err := k.lookupRole(ctx, token, role.ClientID, role.Role)
	if err != nil {
		return "", err
	}
	seen := map[string]bool{}
	var find func(r gocloak.Role, depth int) (string, error)
	find = func(r gocloak.Role, depth int) (string, error) {
		if !gocloak.PBool(r.Composite) || r.ID == nil || seen[*r.ID] {
			return "", nil
		}
		seen[*r.ID] = true
		if depth >= maxCompositeDepth {
			return "composite nested too deep", nil
		}
		children, err := k.gc.GetCompositeRolesByRoleID(ctx, token, k.realm, *r.ID)
		if err != nil {
			return "", mapKCErr(err)
		}
		for _, child := range children {
			if child == nil {
				continue
			}
			if !gocloak.PBool(child.ClientRole) {
				return "realm role " + gocloak.PString(child.Name), nil
			}
			client, err := k.clientIDOf(ctx, token, gocloak.PString(child.ContainerID))
			if err != nil {
				return "", err
			}
			if client == KeycloakAdminClient {
				return gocloak.PString(child.Name), nil
			}
			if got, err := find(*child, depth+1); err != nil || got != "" {
				return got, err
			}
		}
		return "", nil
	}
	return find(r, 0)
}

// clientIDOf is the client id of the client whose Keycloak UUID is uuid,
// kept for the process.
func (k *Keycloak) clientIDOf(ctx context.Context, token, uuid string) (string, error) {
	k.mu.Lock()
	id, ok := k.clientIDsByUUID[uuid]
	k.mu.Unlock()
	if ok {
		return id, nil
	}
	c, err := k.gc.GetClient(ctx, token, k.realm, uuid)
	if err != nil {
		return "", mapKCErr(err)
	}
	if c == nil || c.ClientID == nil {
		return "", ErrNotFound
	}
	k.mu.Lock()
	k.clientIDsByUUID[uuid] = *c.ClientID
	k.mu.Unlock()
	return *c.ClientID, nil
}

// EffectiveClientRoles answers the names of the roles of the Keycloak client
// clientID that the person holds in effect: directly, through their Groups
// and the Groups above them, and through composite roles. It is Keycloak's
// GET /users/{id}/role-mappings/clients/{client}/composite, the roles a token
// for that client carries. A client the realm does not have is
// ErrClientNotFound, a person it does not know ErrNotFound (both are
// ErrNotFound). Any other failure is an error, never an empty list, and no
// error names the person.
//
// It needs view-clients or query-clients (the client's id) and view-users
// (the person's role mappings); service-account-core holds them
// (docs/keycloak-admin-permissions.md). Without them it fails with 403.
func (k *Keycloak) EffectiveClientRoles(ctx context.Context, userID uuid.UUID, clientID string) ([]string, error) {
	token, err := k.accessToken(ctx)
	if err != nil {
		return nil, err
	}
	clientUUID, err := k.clientUUID(ctx, token, clientID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, ErrClientNotFound
		}
		return nil, fmt.Errorf("identity: keycloak client lookup failed: %w", err)
	}
	var roles []*gocloak.Role
	resp, err := k.gc.GetRequestWithBearerAuth(ctx, token).
		SetResult(&roles).
		Get(k.adminBase + "/admin/realms/" + url.PathEscape(k.realm) + "/users/" + userID.String() +
			"/role-mappings/clients/" + url.PathEscape(clientUUID) + "/composite")
	if err != nil {
		// The transport error quotes the address; keep only its cause.
		var addressed *url.Error
		if errors.As(err, &addressed) {
			err = addressed.Err
		}
		return nil, fmt.Errorf("identity: keycloak effective client roles request failed: %w", err)
	}
	switch {
	case resp.StatusCode() == http.StatusNotFound:
		return nil, ErrNotFound
	case resp.IsError():
		return nil, fmt.Errorf("identity: keycloak effective client roles failed with status %d", resp.StatusCode())
	}
	out := make([]string, 0, len(roles))
	for _, role := range roles {
		if role != nil && role.Name != nil {
			out = append(out, *role.Name)
		}
	}
	return out, nil
}

func (k *Keycloak) GroupClientRoles(ctx context.Context, groupID string) ([]ClientRole, error) {
	token, err := k.accessToken(ctx)
	if err != nil {
		return nil, err
	}
	g, err := k.fetchGroup(ctx, token, groupID)
	if err != nil {
		return nil, mapKCErr(err)
	}
	mapping, err := k.gc.GetRoleMappingByGroupID(ctx, token, k.realm, *g.ID)
	if err != nil {
		return nil, mapKCErr(err)
	}
	return clientRolesFromMappings(mapping), nil
}

func (k *Keycloak) SetGroupClientRoles(ctx context.Context, groupID string, roles []ClientRole) error {
	token, err := k.accessToken(ctx)
	if err != nil {
		return err
	}
	g, err := k.fetchGroup(ctx, token, groupID)
	if err != nil {
		return mapKCErr(err)
	}
	current, err := k.gc.GetRoleMappingByGroupID(ctx, token, k.realm, *g.ID)
	if err != nil {
		return mapKCErr(err)
	}
	have := clientRolesFromMappings(current)
	wantKeys := map[string]ClientRole{}
	haveKeys := map[string]ClientRole{}
	clients := map[string]struct{}{}
	for _, r := range roles {
		wantKeys[r.ClientID+"\x00"+r.Role] = r
		clients[r.ClientID] = struct{}{}
	}
	for _, r := range have {
		haveKeys[r.ClientID+"\x00"+r.Role] = r
		clients[r.ClientID] = struct{}{}
	}
	for clientID := range clients {
		var add []gocloak.Role
		var remove []gocloak.Role
		for key, r := range wantKeys {
			if r.ClientID != clientID {
				continue
			}
			if _, ok := haveKeys[key]; ok {
				continue
			}
			// Keycloak administration is never given through core (K3);
			// the service refuses it first with keycloak_admin_role.
			if clientID == KeycloakAdminClient {
				return ErrForbidden
			}
			role, err := k.lookupRole(ctx, token, clientID, r.Role)
			if err != nil {
				return err
			}
			add = append(add, role)
		}
		for key, r := range haveKeys {
			if r.ClientID != clientID {
				continue
			}
			if _, ok := wantKeys[key]; ok {
				continue
			}
			role, err := k.lookupRole(ctx, token, clientID, r.Role)
			if err != nil {
				return err
			}
			remove = append(remove, role)
		}
		clientUUID, err := k.clientUUID(ctx, token, clientID)
		if err != nil {
			return err
		}
		if len(add) > 0 {
			if err := k.gc.AddClientRoleToGroup(ctx, token, k.realm, clientUUID, *g.ID, add); err != nil {
				return mapKCErr(err)
			}
		}
		if len(remove) > 0 {
			if err := k.gc.DeleteClientRoleFromGroup(ctx, token, k.realm, clientUUID, *g.ID, remove); err != nil {
				return mapKCErr(err)
			}
		}
	}
	return nil
}

func (k *Keycloak) UserExtraRoles(ctx context.Context, userID uuid.UUID) ([]ClientRole, error) {
	token, err := k.accessToken(ctx)
	if err != nil {
		return nil, err
	}
	mapping, err := k.gc.GetRoleMappingByUserID(ctx, token, k.realm, userID.String())
	if err != nil {
		return nil, mapKCErr(err)
	}
	return clientRolesFromMappings(mapping), nil
}

func (k *Keycloak) AddUserExtraRole(ctx context.Context, userID uuid.UUID, role ClientRole) error {
	// Keycloak administration is never given through core (K3); the
	// service refuses it first with keycloak_admin_role.
	if role.ClientID == KeycloakAdminClient {
		return ErrForbidden
	}
	token, err := k.accessToken(ctx)
	if err != nil {
		return err
	}
	resolved, err := k.lookupRole(ctx, token, role.ClientID, role.Role)
	if err != nil {
		return err
	}
	clientUUID, err := k.clientUUID(ctx, token, role.ClientID)
	if err != nil {
		return err
	}
	return mapKCErr(k.gc.AddClientRoleToUser(ctx, token, k.realm, clientUUID, userID.String(), []gocloak.Role{resolved}))
}

func (k *Keycloak) RemoveUserExtraRole(ctx context.Context, userID uuid.UUID, role ClientRole) error {
	token, err := k.accessToken(ctx)
	if err != nil {
		return err
	}
	resolved, err := k.lookupRole(ctx, token, role.ClientID, role.Role)
	if err != nil {
		return err
	}
	clientUUID, err := k.clientUUID(ctx, token, role.ClientID)
	if err != nil {
		return err
	}
	return mapKCErr(k.gc.DeleteClientRoleFromUser(ctx, token, k.realm, clientUUID, userID.String(), []gocloak.Role{resolved}))
}

func (k *Keycloak) LogoutAllSessions(ctx context.Context, userID uuid.UUID) error {
	token, err := k.accessToken(ctx)
	if err != nil {
		return err
	}
	return mapKCErr(k.gc.LogoutAllSessions(ctx, token, k.realm, userID.String()))
}

func skipKeycloakClient(id string) bool {
	switch id {
	case "account", "account-console", "admin-cli", "broker", "realm-management", "security-admin-console":
		return true
	default:
		return false
	}
}

func (k *Keycloak) ListClientRoles(ctx context.Context) ([]ClientRole, error) {
	token, err := k.accessToken(ctx)
	if err != nil {
		return nil, err
	}
	clients, err := k.gc.GetClients(ctx, token, k.realm, gocloak.GetClientsParams{Max: gocloak.IntP(200)})
	if err != nil {
		return nil, mapKCErr(err)
	}
	out := make([]ClientRole, 0)
	for _, client := range clients {
		if client == nil || client.ID == nil || client.ClientID == nil {
			continue
		}
		if skipKeycloakClient(*client.ClientID) {
			continue
		}
		roles, err := k.gc.GetClientRoles(ctx, token, k.realm, *client.ID, gocloak.GetRoleParams{Max: gocloak.IntP(500)})
		if err != nil {
			return nil, mapKCErr(err)
		}
		for _, role := range roles {
			if role == nil || role.Name == nil || *role.Name == "" || *role.Name == "uma_protection" {
				continue
			}
			out = append(out, ClientRole{ClientID: *client.ClientID, Role: *role.Name})
		}
	}
	return out, nil
}

// MissingClientRoles reports which of names the Keycloak client clientID does not have, in
// the order given. It only reads (GET /clients and GET /clients/{id}/roles, covered by
// view-clients): core's service account holds no realm-management manage-clients, which would
// let it rewrite every client's redirect URIs and secrets (ADR-0048), so core never creates
// client roles. The Keycloak operator script config/identity-guardrails.sh creates them.
func (k *Keycloak) MissingClientRoles(ctx context.Context, clientID string, names []string) ([]string, error) {
	token, err := k.accessToken(ctx)
	if err != nil {
		return nil, err
	}
	clientUUID, err := k.clientUUID(ctx, token, clientID)
	if err != nil {
		return nil, err
	}
	roles, err := k.gc.GetClientRoles(ctx, token, k.realm, clientUUID, gocloak.GetRoleParams{Max: gocloak.IntP(500)})
	if err != nil {
		return nil, mapKCErr(err)
	}
	present := make(map[string]struct{}, len(roles))
	for _, role := range roles {
		if role != nil && role.Name != nil {
			present[*role.Name] = struct{}{}
		}
	}
	missing := make([]string, 0)
	for _, raw := range names {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		if _, ok := present[name]; ok {
			continue
		}
		present[name] = struct{}{}
		missing = append(missing, name)
	}
	return missing, nil
}

func (k *Keycloak) lookupRole(ctx context.Context, token, clientID, name string) (gocloak.Role, error) {
	clientUUID, err := k.clientUUID(ctx, token, clientID)
	if err != nil {
		return gocloak.Role{}, err
	}
	roles, err := k.gc.GetClientRoles(ctx, token, k.realm, clientUUID, gocloak.GetRoleParams{Max: gocloak.IntP(500)})
	if err != nil {
		return gocloak.Role{}, mapKCErr(err)
	}
	for _, r := range roles {
		if r != nil && r.Name != nil && *r.Name == name {
			return *r, nil
		}
	}
	return gocloak.Role{}, ErrInvalid
}

func (k *Keycloak) clientUUID(ctx context.Context, token, clientID string) (string, error) {
	k.mu.Lock()
	if id, ok := k.clientsByID[clientID]; ok {
		k.mu.Unlock()
		return id, nil
	}
	k.mu.Unlock()
	clients, err := k.gc.GetClients(ctx, token, k.realm, gocloak.GetClientsParams{ClientID: &clientID})
	if err != nil {
		return "", mapKCErr(err)
	}
	for _, c := range clients {
		if c == nil || c.ID == nil || c.ClientID == nil || *c.ClientID != clientID {
			continue
		}
		k.mu.Lock()
		k.clientsByID[clientID] = *c.ID
		k.mu.Unlock()
		return *c.ID, nil
	}
	return "", ErrNotFound
}

func (k *Keycloak) ReadSkyNumber(ctx context.Context, userID uuid.UUID) (string, error) {
	p, err := k.GetUser(ctx, userID)
	if err != nil {
		return "", err
	}
	return p.SkyNumber, nil
}

// skyNumberWrite is the whole body of WriteSkyNumber's PUT: the attribute
// map and the User Profile's root attributes, each as read. `enabled` and
// every other field of the representation are left out on purpose.
type skyNumberWrite struct {
	Username   *string             `json:"username,omitempty"`
	Email      *string             `json:"email,omitempty"`
	FirstName  *string             `json:"firstName,omitempty"`
	LastName   *string             `json:"lastName,omitempty"`
	Attributes map[string][]string `json:"attributes"`
}

// WriteSkyNumber sets the person's skyNumber attribute. It reads the user and
// sends Keycloak only the attribute map (every attribute the read returned,
// values unchanged, with skyNumber set) and the username, e-mail, first and
// last name as read. Keycloak 26.7.4 replaces the attribute map as a whole and
// treats the User Profile's root attributes as part of it: a body with
// `attributes` but without `email`, `firstName` or `lastName` clears them. It
// leaves `enabled`, e-mail verification, required actions, IdP links, groups,
// credentials and role mappings as they are when the body does not name them.
// Leaving `enabled` out keeps an account erasure that disables the person
// between the read and the write from being undone. A user Keycloak does not
// know is ErrNotFound; no error names the subject.
func (k *Keycloak) WriteSkyNumber(ctx context.Context, userID uuid.UUID, skyNumber string) error {
	token, err := k.accessToken(ctx)
	if err != nil {
		return err
	}
	u, err := k.gc.GetUserByID(ctx, token, k.realm, userID.String())
	if err != nil {
		return mapKCErr(err)
	}
	attrs := map[string][]string{}
	if u.Attributes != nil {
		for key, vals := range *u.Attributes {
			attrs[key] = vals
		}
	}
	attrs["skyNumber"] = []string{skyNumber}
	resp, err := k.gc.GetRequestWithBearerAuth(ctx, token).
		SetBody(skyNumberWrite{
			Username: u.Username, Email: u.Email, FirstName: u.FirstName, LastName: u.LastName,
			Attributes: attrs,
		}).
		Put(k.adminBase + "/admin/realms/" + url.PathEscape(k.realm) + "/users/" + userID.String())
	if err != nil {
		return errors.New("identity: keycloak sky number write request failed")
	}
	switch {
	case !resp.IsError():
		return nil
	case resp.StatusCode() == http.StatusNotFound:
		return ErrNotFound
	case resp.StatusCode() == http.StatusConflict:
		return ErrInvalid
	default:
		return fmt.Errorf("identity: keycloak sky number write failed with status %d", resp.StatusCode())
	}
}
