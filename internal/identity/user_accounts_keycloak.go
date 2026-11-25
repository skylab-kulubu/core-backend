package identity

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"
)

// The person card's Keycloak Admin REST calls (docs/user-accounts.md,
// docs/keycloak-admin-permissions.md). Reads need view-users, writes
// manage-users, the sign-in events view-events. No error names the person,
// an address or a password: the person's id is in the request address and
// Keycloak's error body may hold anything, so only the status (and, for a
// refused password, the policy's message key) is kept.

// adminCall sends one Admin REST request to path under the realm, with
// body as JSON when not nil, and decodes a 2xx answer into out when not
// nil. It answers the status; a status of 400 or more is not an error here.
func (k *Keycloak) adminCall(ctx context.Context, what, method, path string, query url.Values, body, out any) (int, []byte, error) {
	token, err := k.accessToken(ctx)
	if err != nil {
		return 0, nil, fmt.Errorf("identity: %s could not get an admin token", what)
	}
	u := k.adminBase + "/admin/realms/" + url.PathEscape(k.realm) + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, fmt.Errorf("identity: %s failed", what)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, reader)
	if err != nil {
		return 0, nil, fmt.Errorf("identity: %s failed", what)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := k.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return 0, nil, fmt.Errorf("identity: %s: %w", what, ctx.Err())
		}
		return 0, nil, fmt.Errorf("identity: %s failed", what)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("identity: %s failed", what)
	}
	if resp.StatusCode < 300 && out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, nil, fmt.Errorf("identity: %s answered an unexpected body", what)
		}
	}
	return resp.StatusCode, raw, nil
}

// statusError is a non-2xx answer as an error: 404 ErrNotFound, 409
// ErrInvalid, anything else the status alone.
func statusError(what string, status int) error {
	switch {
	case status < 300:
		return nil
	case status == http.StatusNotFound:
		return ErrNotFound
	case status == http.StatusConflict:
		return ErrInvalid
	default:
		return &keycloakStatusError{what: what, status: status}
	}
}

// keycloakStatusError is a Keycloak answer with a status that tells
// nothing more; the audit record keeps it as keycloak_<status>.
type keycloakStatusError struct {
	what   string
	status int
}

func (e *keycloakStatusError) Error() string {
	return fmt.Sprintf("identity: %s failed with status %d", e.what, e.status)
}

func userPath(id uuid.UUID) string { return "/users/" + id.String() }

func millis(ms int64) *time.Time {
	if ms <= 0 {
		return nil
	}
	t := time.UnixMilli(ms).UTC()
	return &t
}

// accountRepresentation is the part of Keycloak's UserRepresentation the
// card reads.
type accountRepresentation struct {
	ID                     string              `json:"id"`
	Username               *string             `json:"username,omitempty"`
	Email                  *string             `json:"email,omitempty"`
	EmailVerified          *bool               `json:"emailVerified,omitempty"`
	FirstName              *string             `json:"firstName,omitempty"`
	LastName               *string             `json:"lastName,omitempty"`
	Enabled                *bool               `json:"enabled,omitempty"`
	CreatedTimestamp       int64               `json:"createdTimestamp,omitempty"`
	ServiceAccountClientID string              `json:"serviceAccountClientId,omitempty"`
	RequiredActions        []string            `json:"requiredActions,omitempty"`
	Attributes             map[string][]string `json:"attributes,omitempty"`
}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func (k *Keycloak) accountRepresentation(ctx context.Context, id uuid.UUID) (accountRepresentation, error) {
	var rep accountRepresentation
	status, _, err := k.adminCall(ctx, "account read", http.MethodGet, userPath(id), nil, nil, &rep)
	if err != nil {
		return rep, err
	}
	return rep, statusError("account read", status)
}

func (k *Keycloak) AccountUser(ctx context.Context, id uuid.UUID) (AccountUser, error) {
	rep, err := k.accountRepresentation(ctx, id)
	if err != nil {
		return AccountUser{}, err
	}
	return AccountUser{
		ID: id, Username: str(rep.Username), Email: str(rep.Email),
		EmailVerified:   rep.EmailVerified != nil && *rep.EmailVerified,
		FirstName:       str(rep.FirstName),
		LastName:        str(rep.LastName),
		Enabled:         rep.Enabled != nil && *rep.Enabled,
		ServiceAccount:  rep.ServiceAccountClientID != "" || strings.HasPrefix(str(rep.Username), serviceAccountPrefix),
		CreatedAt:       millis(rep.CreatedTimestamp),
		RequiredActions: rep.RequiredActions,
		Attributes:      rep.Attributes,
	}, nil
}

func (k *Keycloak) AccountCredentials(ctx context.Context, id uuid.UUID) ([]AccountCredential, error) {
	var rows []struct {
		ID          string `json:"id"`
		Type        string `json:"type"`
		UserLabel   string `json:"userLabel"`
		CreatedDate int64  `json:"createdDate"`
	}
	status, _, err := k.adminCall(ctx, "credential read", http.MethodGet, userPath(id)+"/credentials", nil, nil, &rows)
	if err != nil {
		return nil, err
	}
	if err := statusError("credential read", status); err != nil {
		return nil, err
	}
	out := make([]AccountCredential, 0, len(rows))
	for _, row := range rows {
		out = append(out, AccountCredential{ID: row.ID, Type: row.Type, Label: row.UserLabel, CreatedAt: millis(row.CreatedDate)})
	}
	return out, nil
}

func (k *Keycloak) LinkedIdentities(ctx context.Context, id uuid.UUID) ([]LinkedIdentity, error) {
	var rows []struct {
		IdentityProvider string `json:"identityProvider"`
		UserName         string `json:"userName"`
	}
	status, _, err := k.adminCall(ctx, "identity provider link read", http.MethodGet, userPath(id)+"/federated-identity", nil, nil, &rows)
	if err != nil {
		return nil, err
	}
	if err := statusError("identity provider link read", status); err != nil {
		return nil, err
	}
	out := make([]LinkedIdentity, 0, len(rows))
	for _, row := range rows {
		out = append(out, LinkedIdentity{Provider: row.IdentityProvider, UserName: row.UserName})
	}
	return out, nil
}

func (k *Keycloak) AccountSessions(ctx context.Context, id uuid.UUID) ([]AccountSession, error) {
	var rows []struct {
		Start      int64             `json:"start"`
		LastAccess int64             `json:"lastAccess"`
		Clients    map[string]string `json:"clients"`
	}
	status, _, err := k.adminCall(ctx, "session read", http.MethodGet, userPath(id)+"/sessions", nil, nil, &rows)
	if err != nil {
		return nil, err
	}
	if err := statusError("session read", status); err != nil {
		return nil, err
	}
	out := make([]AccountSession, 0, len(rows))
	for _, row := range rows {
		session := AccountSession{Clients: make([]string, 0, len(row.Clients))}
		if t := millis(row.Start); t != nil {
			session.Start = *t
		}
		if t := millis(row.LastAccess); t != nil {
			session.LastAccess = *t
		}
		for _, client := range row.Clients {
			session.Clients = append(session.Clients, client)
		}
		slices.Sort(session.Clients)
		out = append(out, session)
	}
	slices.SortFunc(out, func(a, b AccountSession) int { return b.LastAccess.Compare(a.LastAccess) })
	return out, nil
}

func (k *Keycloak) Lockout(ctx context.Context, id uuid.UUID) (AccountLockout, error) {
	var row struct {
		NumFailures int   `json:"numFailures"`
		Disabled    bool  `json:"disabled"`
		LastFailure int64 `json:"lastFailure"`
	}
	status, _, err := k.adminCall(ctx, "lock read", http.MethodGet, "/attack-detection/brute-force/users/"+id.String(), nil, nil, &row)
	if err != nil {
		return AccountLockout{}, err
	}
	if err := statusError("lock read", status); err != nil {
		return AccountLockout{}, err
	}
	return AccountLockout{Locked: row.Disabled, Failures: row.NumFailures, LastFailure: millis(row.LastFailure)}, nil
}

// loginEventTypes are the sign-in events the card lists.
var loginEventTypes = []string{"LOGIN", "LOGIN_ERROR", "LOGOUT", "UPDATE_PASSWORD", "UPDATE_CREDENTIAL", "REMOVE_CREDENTIAL", "UPDATE_EMAIL", "VERIFY_EMAIL"}

func (k *Keycloak) LoginEvents(ctx context.Context, id uuid.UUID, limit int) ([]LoginEvent, error) {
	query := url.Values{"user": {id.String()}, "first": {"0"}, "max": {strconv.Itoa(limit)}, "type": loginEventTypes}
	var rows []struct {
		Time     int64  `json:"time"`
		Type     string `json:"type"`
		ClientID string `json:"clientId"`
		Error    string `json:"error"`
	}
	status, _, err := k.adminCall(ctx, "sign-in event read", http.MethodGet, "/events", query, nil, &rows)
	if err != nil {
		return nil, err
	}
	if status == http.StatusForbidden || status == http.StatusUnauthorized {
		return nil, ErrEventsUnavailable
	}
	if err := statusError("sign-in event read", status); err != nil {
		return nil, err
	}
	out := make([]LoginEvent, 0, len(rows))
	for _, row := range rows {
		event := LoginEvent{Type: row.Type, Client: row.ClientID, Error: safeCode(row.Error)}
		if t := millis(row.Time); t != nil {
			event.Time = *t
		}
		out = append(out, event)
	}
	slices.SortStableFunc(out, func(a, b LoginEvent) int { return b.Time.Compare(a.Time) })
	return out, nil
}

var safeCodePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// safeCode is code when it is a plain identifier (an error or message key),
// "" otherwise.
func safeCode(code string) string {
	if safeCodePattern.MatchString(code) {
		return code
	}
	return ""
}

// AddressHolders reads the accounts that hold address: as their Primary
// e-mail (GET /users?email=…&exact=true) or their School or Personal e-mail
// attribute (GET /users?q=<attribute>:…), all with view-users.
func (k *Keycloak) AddressHolders(ctx context.Context, address string) ([]uuid.UUID, error) {
	if address == "" || strings.ContainsAny(address, " \t\r\n:") {
		return nil, nil
	}
	var out []uuid.UUID
	add := func(rows []json.RawMessage, keep func(row accountRepresentation) bool) {
		for _, raw := range rows {
			var row accountRepresentation
			if json.Unmarshal(raw, &row) != nil {
				continue
			}
			id, err := uuid.Parse(row.ID)
			if err != nil || slices.Contains(out, id) || !keep(row) {
				continue
			}
			out = append(out, id)
		}
	}
	query := pageQuery(0, maxPersonalEmailHolders, false)
	query.Set("email", address)
	query.Set("exact", "true")
	rows, err := k.adminUsersPage(ctx, "address holder search", query)
	if err != nil {
		return nil, err
	}
	add(rows, func(row accountRepresentation) bool {
		return strings.EqualFold(strings.TrimSpace(str(row.Email)), address)
	})
	for _, attribute := range []string{schoolEmailAttribute, personalEmailAttribute} {
		query := pageQuery(0, maxPersonalEmailHolders, false)
		query.Set("q", attribute+":"+address)
		rows, err := k.adminUsersPage(ctx, "address holder search", query)
		if err != nil {
			return nil, err
		}
		add(rows, func(row accountRepresentation) bool {
			return slices.ContainsFunc(row.Attributes[attribute], func(v string) bool { return strings.EqualFold(strings.TrimSpace(v), address) })
		})
	}
	return out, nil
}

// SetEnabled sends Keycloak exactly {"enabled": …}: Keycloak leaves every
// other field as it is. Unlike DisableUser it carries no erasure header, so
// Keycloak does not mark the person as being erased.
func (k *Keycloak) SetEnabled(ctx context.Context, id uuid.UUID, enabled bool) error {
	status, _, err := k.adminCall(ctx, "account enable", http.MethodPut, userPath(id), nil, map[string]bool{"enabled": enabled}, nil)
	if err != nil {
		return err
	}
	return statusError("account enable", status)
}

// SetTemporaryPassword is PUT /users/{id}/reset-password with temporary
// true: Keycloak adds UPDATE_PASSWORD. Its admin event names no password. A
// password the realm's policy refuses is an AccountError with the policy's
// message key.
func (k *Keycloak) SetTemporaryPassword(ctx context.Context, id uuid.UUID, password string) error {
	body := map[string]any{"type": "password", "value": password, "temporary": true}
	status, raw, err := k.adminCall(ctx, "password set", http.MethodPut, userPath(id)+"/reset-password", nil, body, nil)
	if err != nil {
		return err
	}
	if status == http.StatusBadRequest {
		var problem struct {
			Error        string `json:"error"`
			ErrorMessage string `json:"errorMessage"`
		}
		_ = json.Unmarshal(raw, &problem)
		key := problem.Error
		if key == "" {
			key = problem.ErrorMessage
		}
		return &AccountError{Status: 422, Code: AccountPasswordPolicy, Policy: safeCode(key)}
	}
	return statusError("password set", status)
}

// SendActionsMail is PUT /users/{id}/execute-actions-email: Keycloak mails
// the person a link (SkyNotify's keycloak.generic) that runs actions. Without
// an address Keycloak answers 400.
func (k *Keycloak) SendActionsMail(ctx context.Context, id uuid.UUID, actions []string) error {
	status, _, err := k.adminCall(ctx, "actions mail", http.MethodPut, userPath(id)+"/execute-actions-email", nil, actions, nil)
	if err != nil {
		return err
	}
	return mailStatus("actions mail", status)
}

// SendVerificationMail is PUT /users/{id}/send-verify-email (SkyNotify's
// keycloak.verify-email).
func (k *Keycloak) SendVerificationMail(ctx context.Context, id uuid.UUID) error {
	status, _, err := k.adminCall(ctx, "verification mail", http.MethodPut, userPath(id)+"/send-verify-email", nil, nil, nil)
	if err != nil {
		return err
	}
	return mailStatus("verification mail", status)
}

func mailStatus(what string, status int) error {
	switch {
	case status < 300:
		return nil
	case status == http.StatusBadRequest:
		return &AccountError{Status: 409, Code: AccountNoEmail}
	case status >= 500:
		return &AccountError{Status: 502, Code: AccountMailFailed}
	default:
		return statusError(what, status)
	}
}

// accountWrite is the body of UpdateAccount's PUT when the write leaves
// the User Profile's attributes alone: only the flags it names.
type accountWrite struct {
	EmailVerified   *bool     `json:"emailVerified,omitempty"`
	RequiredActions *[]string `json:"requiredActions,omitempty"`
}

// accountProfileWrite is the body when the write changes names, the e-mail
// or attributes: all of them, as read and changed. Keycloak 26.7.4 replaces
// the attribute map as a whole and clears the root attributes such a body
// leaves out (WriteSkyNumber); the username is read-only
// (editUsernameAllowed=false) and sent as read. `enabled` is never in it,
// so an erasure that disables the person between the read and the write is
// not undone.
type accountProfileWrite struct {
	Username        *string             `json:"username,omitempty"`
	Email           *string             `json:"email,omitempty"`
	FirstName       *string             `json:"firstName,omitempty"`
	LastName        *string             `json:"lastName,omitempty"`
	EmailVerified   *bool               `json:"emailVerified,omitempty"`
	RequiredActions *[]string           `json:"requiredActions,omitempty"`
	Attributes      map[string][]string `json:"attributes"`
}

func (k *Keycloak) UpdateAccount(ctx context.Context, id uuid.UUID, change AccountUpdate) error {
	if !change.touchesProfile() {
		return k.putAccount(ctx, id, accountWrite{EmailVerified: change.EmailVerified, RequiredActions: change.RequiredActions}, change)
	}
	rep, err := k.accountRepresentation(ctx, id)
	if err != nil {
		return err
	}
	attributes := map[string][]string{}
	for name, values := range rep.Attributes {
		attributes[name] = values
	}
	for name, value := range change.Attributes {
		if value == "" {
			delete(attributes, name)
		} else {
			attributes[name] = []string{value}
		}
	}
	body := accountProfileWrite{
		Username: rep.Username, Email: rep.Email, FirstName: rep.FirstName, LastName: rep.LastName,
		EmailVerified: change.EmailVerified, RequiredActions: change.RequiredActions, Attributes: attributes,
	}
	if change.Email != nil {
		body.Email = change.Email
	}
	if change.FirstName != nil {
		body.FirstName = change.FirstName
	}
	if change.LastName != nil {
		body.LastName = change.LastName
	}
	return k.putAccount(ctx, id, body, change)
}

// putAccount sends body and names a refusal by what change writes: Keycloak
// answers 400 when its User Profile refuses a value (an address, a name
// with a character its validators refuse) and 409 when an address is taken.
func (k *Keycloak) putAccount(ctx context.Context, id uuid.UUID, body any, change AccountUpdate) error {
	status, _, err := k.adminCall(ctx, "account write", http.MethodPut, userPath(id), nil, body, nil)
	if err != nil {
		return err
	}
	switch {
	case status == http.StatusConflict && change.Email != nil:
		return &AccountError{Status: 409, Code: AccountEmailTaken}
	case status == http.StatusBadRequest && change.Email != nil:
		return &AccountError{Status: 422, Code: AccountEmailInvalid}
	case status == http.StatusBadRequest && (change.FirstName != nil || change.LastName != nil):
		return &AccountError{Status: 422, Code: AccountNameInvalid}
	case status == http.StatusBadRequest:
		return &AccountError{Status: 422, Code: AccountInvalid}
	}
	return statusError("account write", status)
}

func (k *Keycloak) DeleteCredential(ctx context.Context, id uuid.UUID, credentialID string) error {
	status, _, err := k.adminCall(ctx, "credential delete", http.MethodDelete, userPath(id)+"/credentials/"+url.PathEscape(credentialID), nil, nil, nil)
	if err != nil {
		return err
	}
	return statusError("credential delete", status)
}

func (k *Keycloak) UnlinkIdentity(ctx context.Context, id uuid.UUID, provider string) error {
	status, _, err := k.adminCall(ctx, "identity provider unlink", http.MethodDelete, userPath(id)+"/federated-identity/"+url.PathEscape(provider), nil, nil, nil)
	if err != nil {
		return err
	}
	return statusError("identity provider unlink", status)
}

func (k *Keycloak) ClearLockout(ctx context.Context, id uuid.UUID) error {
	status, _, err := k.adminCall(ctx, "lock clear", http.MethodDelete, "/attack-detection/brute-force/users/"+id.String(), nil, nil, nil)
	if err != nil {
		return err
	}
	return statusError("lock clear", status)
}

// heldRoleClientsTTL is how long the realm's client list HeldRoles walks is
// kept: clients are added seldom, and a new one is seen within this long.
const heldRoleClientsTTL = 10 * time.Minute

// selfServiceClients hold no role over anyone else: the account console's
// roles are one's own account, the other two hold none.
var selfServiceClients = []string{"account", "account-console", "admin-cli", "security-admin-console"}

type heldRoleClient struct{ uuid, clientID string }

// heldRoleClients are the realm's clients HeldRoles reads (GET /clients,
// view-clients), kept heldRoleClientsTTL.
func (k *Keycloak) heldRoleClients(ctx context.Context) ([]heldRoleClient, error) {
	k.mu.Lock()
	if k.heldClients != nil && time.Now().Before(k.heldClientsUntil) {
		out := k.heldClients
		k.mu.Unlock()
		return out, nil
	}
	k.mu.Unlock()
	var rows []struct {
		ID       string `json:"id"`
		ClientID string `json:"clientId"`
	}
	status, _, err := k.adminCall(ctx, "client list", http.MethodGet, "/clients", url.Values{"first": {"0"}, "max": {"1000"}}, nil, &rows)
	if err != nil {
		return nil, err
	}
	if err := statusError("client list", status); err != nil {
		return nil, err
	}
	out := make([]heldRoleClient, 0, len(rows))
	for _, row := range rows {
		if row.ID == "" || row.ClientID == "" || slices.Contains(selfServiceClients, row.ClientID) {
			continue
		}
		out = append(out, heldRoleClient{uuid: row.ID, clientID: row.ClientID})
	}
	k.mu.Lock()
	k.heldClients, k.heldClientsUntil = out, time.Now().Add(heldRoleClientsTTL)
	k.mu.Unlock()
	return out, nil
}

// HeldRoles reads every role the person holds in effect (HeldRoleReader):
// GET /users/{id}/role-mappings/realm/composite and, for each client but
// the self-service ones, GET /users/{id}/role-mappings/clients/{client}/
// composite (view-users). Keycloak resolves the Groups above the person's,
// composites (a realm role's client roles included) and the default role on
// its side, as for a Group in KeycloakAdminRole. A failure on any read is an
// error, never a shorter list.
func (k *Keycloak) HeldRoles(ctx context.Context, id uuid.UUID) ([]ClientRole, error) {
	clients, err := k.heldRoleClients(ctx)
	if err != nil {
		return nil, err
	}
	type named struct {
		Name string `json:"name"`
	}
	read := func(what, path string, clientID string) ([]ClientRole, error) {
		var rows []named
		status, _, err := k.adminCall(ctx, what, http.MethodGet, userPath(id)+path, nil, nil, &rows)
		if err != nil {
			return nil, err
		}
		if err := statusError(what, status); err != nil {
			return nil, err
		}
		out := make([]ClientRole, 0, len(rows))
		for _, row := range rows {
			if row.Name != "" {
				out = append(out, ClientRole{ClientID: clientID, Role: row.Name})
			}
		}
		return out, nil
	}
	held, err := read("held realm roles", "/role-mappings/realm/composite", "")
	if err != nil {
		return nil, err
	}
	results := make([][]ClientRole, len(clients))
	group, gctx := errgroup.WithContext(ctx)
	group.SetLimit(6)
	for i, client := range clients {
		group.Go(func() error {
			var rows []named
			status, _, err := k.adminCall(gctx, "held client roles", http.MethodGet,
				userPath(id)+"/role-mappings/clients/"+url.PathEscape(client.uuid)+"/composite", nil, nil, &rows)
			if err != nil {
				return err
			}
			if status == http.StatusNotFound {
				// The client went away since the list was read.
				return nil
			}
			if err := statusError("held client roles", status); err != nil {
				return err
			}
			for _, row := range rows {
				if row.Name != "" {
					results[i] = append(results[i], ClientRole{ClientID: client.clientID, Role: row.Name})
				}
			}
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return nil, err
	}
	for _, roles := range results {
		held = append(held, roles...)
	}
	return held, nil
}
