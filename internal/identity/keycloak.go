package identity

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Nerzal/gocloak/v13"
	"github.com/google/uuid"
)

type KeycloakConfig struct {
	URL          string
	Realm        string
	ClientID     string
	ClientSecret string
}

type Keycloak struct {
	gc           *gocloak.GoCloak
	base         string
	realm        string
	clientID     string
	clientSecret string

	mu          sync.Mutex
	token       string
	tokenExpiry time.Time
	clientsByID map[string]string
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
	return &Keycloak{
		gc:           gocloak.NewClient(base),
		base:         base,
		realm:        realm,
		clientID:     cfg.ClientID,
		clientSecret: cfg.ClientSecret,
		clientsByID:  map[string]string{},
	}
}

func (k *Keycloak) accessToken(ctx context.Context) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.token != "" && time.Now().Before(k.tokenExpiry) {
		return k.token, nil
	}
	jwt, err := k.gc.LoginClient(ctx, k.clientID, k.clientSecret, k.realm)
	if err != nil {
		return "", err
	}
	k.token = jwt.AccessToken
	ttl := time.Duration(jwt.ExpiresIn-30) * time.Second
	if ttl < time.Second {
		ttl = 30 * time.Second
	}
	k.tokenExpiry = time.Now().Add(ttl)
	return k.token, nil
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
			Get(k.base + "/admin/realms/" + k.realm + "/groups/" + groupID + "/children")
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
	client := &http.Client{Timeout: 60 * time.Second}
	for {
		u := k.base + "/admin/realms/" + k.realm + "/users?first=" + strconv.Itoa(first) + "&max=" + strconv.Itoa(pageSize) + "&briefRepresentation=true"
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := client.Do(req)
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
				LastName: row.LastName, Username: row.Username,
			})
		}
		if len(chunks) < pageSize {
			return out, nil
		}
		first += len(chunks)
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, k.base+"/admin/realms/"+k.realm+"/users?"+params.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
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
			LastName: row.LastName, Username: row.Username,
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
	base := k.base + "/admin/realms/" + k.realm + "/clients/" + clientUUID + "/roles/" + url.PathEscape(role)
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

func (k *Keycloak) DeleteUser(ctx context.Context, id uuid.UUID) error {
	token, err := k.accessToken(ctx)
	if err != nil {
		return err
	}
	return mapKCErr(k.gc.DeleteUser(ctx, token, k.realm, id.String()))
}

func (k *Keycloak) GroupsForUser(ctx context.Context, userID uuid.UUID) ([]Group, error) {
	token, err := k.accessToken(ctx)
	if err != nil {
		return nil, err
	}
	gs, err := k.gc.GetUserGroups(ctx, token, k.realm, userID.String(), gocloak.GetGroupsParams{
		Full: gocloak.BoolP(true),
		Max:  gocloak.IntP(1000),
	})
	if err != nil {
		return nil, mapKCErr(err)
	}
	return flattenGroups(gs), nil
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
	u.Attributes = &attrs
	return mapKCErr(k.gc.UpdateUser(ctx, token, k.realm, *u))
}
