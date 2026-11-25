package identity

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/Nerzal/gocloak/v13"
	"github.com/google/uuid"
)

// AdminEvent is one of Keycloak's admin events (GET …/admin-events), as the
// console audit reads it (docs/console-audit.md): ids, Keycloak's names and
// the representation of the change. Keycloak's details (a user name) and the
// author's IP address are not read.
type AdminEvent struct {
	ID          string
	Time        int64
	RealmID     string
	AuthRealmID string
	// AuthClientID is the internal id (a UUID) of the client the author
	// signed in with, in the author's realm: the console's
	// security-admin-console, or kcadm's admin-cli.
	AuthClientID  string
	AuthUserID    string
	OperationType string
	ResourceType  string
	ResourcePath  string
	// Representation is the change's JSON, when the realm keeps it
	// (adminEventsDetailsEnabled); empty otherwise.
	Representation string
	// Error is set on an event of a write that failed.
	Error string
}

// ErrAdminEventsUnavailable is Keycloak refusing the admin event read:
// core's service account lacks view-events.
var ErrAdminEventsUnavailable = errors.New("identity: admin events unavailable")

// AdminEvents reads the admin events from fromMillis (inclusive), oldest
// first, at most max, of resourceTypes (view-events).
func (k *Keycloak) AdminEvents(ctx context.Context, fromMillis int64, max int, resourceTypes []string) ([]AdminEvent, error) {
	query := url.Values{
		"dateFrom":  {strconv.FormatInt(fromMillis, 10)},
		"direction": {"asc"},
		"first":     {"0"},
		"max":       {strconv.Itoa(max)},
	}
	if len(resourceTypes) > 0 {
		query["resourceTypes"] = resourceTypes
	}
	var rows []struct {
		ID          string `json:"id"`
		Time        int64  `json:"time"`
		RealmID     string `json:"realmId"`
		AuthDetails *struct {
			RealmID  string `json:"realmId"`
			ClientID string `json:"clientId"`
			UserID   string `json:"userId"`
		} `json:"authDetails"`
		OperationType  string `json:"operationType"`
		ResourceType   string `json:"resourceType"`
		ResourcePath   string `json:"resourcePath"`
		Representation string `json:"representation"`
		Error          string `json:"error"`
	}
	status, _, err := k.adminCall(ctx, "admin event read", http.MethodGet, "/admin-events", query, nil, &rows)
	if err != nil {
		return nil, err
	}
	if status == http.StatusForbidden || status == http.StatusUnauthorized {
		return nil, ErrAdminEventsUnavailable
	}
	if err := statusError("admin event read", status); err != nil {
		return nil, err
	}
	out := make([]AdminEvent, 0, len(rows))
	for _, row := range rows {
		e := AdminEvent{
			ID: row.ID, Time: row.Time, RealmID: row.RealmID,
			OperationType: row.OperationType, ResourceType: row.ResourceType, ResourcePath: row.ResourcePath,
			Representation: row.Representation, Error: row.Error,
		}
		if row.AuthDetails != nil {
			e.AuthRealmID, e.AuthClientID, e.AuthUserID = row.AuthDetails.RealmID, row.AuthDetails.ClientID, row.AuthDetails.UserID
		}
		out = append(out, e)
	}
	return out, nil
}

// ServiceAccountSubject is the subject of core's own service account: the
// sub of the token core holds. Keycloak names it as the author of every
// admin event core's writes make.
func (k *Keycloak) ServiceAccountSubject(ctx context.Context) (uuid.UUID, error) {
	token, err := k.accessToken(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	return tokenSubject(token)
}

// tokenSubject is the sub claim of a JWT, read without checking its
// signature: core's own token, just issued to it.
func tokenSubject(token string) (uuid.UUID, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return uuid.Nil, errors.New("identity: service account token is not a JWT")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return uuid.Nil, errors.New("identity: service account token payload")
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		return uuid.Nil, errors.New("identity: service account token payload")
	}
	id, err := uuid.Parse(claims.Sub)
	if err != nil {
		return uuid.Nil, errors.New("identity: service account token has no subject")
	}
	return id, nil
}

// ClientInfo is what the console audit checks of a reconciler's client.
type ClientInfo struct {
	// ID is Keycloak's internal id (a UUID).
	ID string
	// Public is a public client (no secret: anyone may sign in through
	// it); ServiceAccounts a client with a service account.
	Public          bool
	ServiceAccounts bool
}

// ClientInfoOf reads the realm's client clientID (view-clients); ErrNotFound
// when there is none.
func (k *Keycloak) ClientInfoOf(ctx context.Context, clientID string) (ClientInfo, error) {
	token, err := k.accessToken(ctx)
	if err != nil {
		return ClientInfo{}, err
	}
	clients, err := k.gc.GetClients(ctx, token, k.realm, gocloak.GetClientsParams{ClientID: &clientID})
	if err != nil {
		return ClientInfo{}, mapKCErr(err)
	}
	for _, c := range clients {
		if c == nil || c.ID == nil || c.ClientID == nil || *c.ClientID != clientID {
			continue
		}
		return ClientInfo{
			ID:              *c.ID,
			Public:          c.PublicClient != nil && *c.PublicClient,
			ServiceAccounts: c.ServiceAccountsEnabled != nil && *c.ServiceAccountsEnabled,
		}, nil
	}
	return ClientInfo{}, ErrNotFound
}

// ClientIDOf is the client id of the client whose Keycloak UUID is
// clientUUID (view-clients), kept for the process.
func (k *Keycloak) ClientIDOf(ctx context.Context, clientUUID string) (string, error) {
	token, err := k.accessToken(ctx)
	if err != nil {
		return "", err
	}
	return k.clientIDOf(ctx, token, clientUUID)
}
