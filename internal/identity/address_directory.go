package identity

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

// AddressHolder is one account as the verified-address directory of the
// recipient question reads it (commpref.Recipients, docs/news-audience.md,
// "Recipients"): the Primary e-mail with its flag, the School and Personal
// e-mail attributes (erasureAddressAttributes) with the Personal e-mail's
// proof stamp, whether Keycloak has the account enabled, and whether it is a
// client's service account, never a person to write to. Each value is the
// attribute's first, raw, as Keycloak holds it.
type AddressHolder struct {
	ID                      uuid.UUID
	Enabled                 bool
	Email                   string
	EmailVerified           bool
	SchoolEmail             string
	PersonalEmail           string
	PersonalEmailVerifiedAt string
	ServiceAccount          bool
}

// AddressHolderPage is one page of the realm's accounts. Rows counts every
// row Keycloak answered, read or not: a page with fewer Rows than asked is
// the last.
type AddressHolderPage struct {
	Holders []AddressHolder
	Rows    int
}

// LinkedPage is one page of the accounts linked to an identity provider.
// Rows counts every row Keycloak answered, read or not.
type LinkedPage struct {
	IDs  []uuid.UUID
	Rows int
}

const (
	schoolEmailAttribute             = "schoolEmail"
	personalEmailAttribute           = "personalEmail"
	personalEmailVerifiedAtAttribute = "personalEmailVerifiedAt"
)

// adminUsersPage reads one page of GET /admin/realms/<realm>/users with
// query. No error names a person: the body Keycloak answers may hold
// anything, so only the status is kept.
func (k *Keycloak) adminUsersPage(ctx context.Context, what string, query url.Values) ([]json.RawMessage, error) {
	token, err := k.accessToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("identity: %s could not get an admin token", what)
	}
	u := k.adminBase + "/admin/realms/" + url.PathEscape(k.realm) + "/users?" + query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("identity: %s failed", what)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := k.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("identity: %s: %w", what, ctx.Err())
		}
		return nil, fmt.Errorf("identity: %s failed", what)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("identity: %s failed", what)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("identity: %s failed with status %d", what, resp.StatusCode)
	}
	var rows []json.RawMessage
	if err := json.Unmarshal(body, &rows); err != nil {
		return nil, fmt.Errorf("identity: %s answered no user list", what)
	}
	return rows, nil
}

func pageQuery(first, max int, brief bool) url.Values {
	return url.Values{
		"first":               {strconv.Itoa(first)},
		"max":                 {strconv.Itoa(max)},
		"briefRepresentation": {strconv.FormatBool(brief)},
	}
}

// AddressHoldersPage reads one page of the realm's accounts in full
// representation (`GET /users?briefRepresentation=false`, view-users: the
// read YTUAttributes already makes), from first for at most max. A row that
// is not a user (no id, not a UUID) is counted in Rows and left out.
func (k *Keycloak) AddressHoldersPage(ctx context.Context, first, max int) (AddressHolderPage, error) {
	if first < 0 || max <= 0 {
		return AddressHolderPage{}, ErrInvalid
	}
	rows, err := k.adminUsersPage(ctx, "address directory read", pageQuery(first, max, false))
	if err != nil {
		return AddressHolderPage{}, err
	}
	page := AddressHolderPage{Holders: make([]AddressHolder, 0, len(rows)), Rows: len(rows)}
	for _, raw := range rows {
		var row struct {
			ID                     string              `json:"id"`
			Username               string              `json:"username"`
			Enabled                *bool               `json:"enabled"`
			Email                  string              `json:"email"`
			EmailVerified          *bool               `json:"emailVerified"`
			ServiceAccountClientID string              `json:"serviceAccountClientId"`
			Attributes             map[string][]string `json:"attributes"`
		}
		if err := json.Unmarshal(raw, &row); err != nil {
			continue
		}
		id, err := uuid.Parse(row.ID)
		if err != nil {
			continue
		}
		first := func(name string) string {
			if values := row.Attributes[name]; len(values) > 0 {
				return values[0]
			}
			return ""
		}
		page.Holders = append(page.Holders, AddressHolder{
			ID:                      id,
			Enabled:                 row.Enabled != nil && *row.Enabled,
			Email:                   row.Email,
			EmailVerified:           row.EmailVerified != nil && *row.EmailVerified,
			SchoolEmail:             first(schoolEmailAttribute),
			PersonalEmail:           first(personalEmailAttribute),
			PersonalEmailVerifiedAt: first(personalEmailVerifiedAtAttribute),
			ServiceAccount:          row.ServiceAccountClientID != "" || strings.HasPrefix(row.Username, serviceAccountPrefix),
		})
	}
	return page, nil
}

// IdentityProviderLinkedPage reads one page of the accounts linked to the
// identity provider alias (`GET /users?idpAlias=<alias>`, brief: only the id
// is read), from first for at most max.
func (k *Keycloak) IdentityProviderLinkedPage(ctx context.Context, alias string, first, max int) (LinkedPage, error) {
	if first < 0 || max <= 0 || strings.TrimSpace(alias) == "" {
		return LinkedPage{}, ErrInvalid
	}
	query := pageQuery(first, max, true)
	query.Set("idpAlias", alias)
	rows, err := k.adminUsersPage(ctx, "identity provider link read", query)
	if err != nil {
		return LinkedPage{}, err
	}
	page := LinkedPage{IDs: make([]uuid.UUID, 0, len(rows)), Rows: len(rows)}
	for _, raw := range rows {
		var row struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(raw, &row); err != nil {
			continue
		}
		if id, err := uuid.Parse(row.ID); err == nil {
			page.IDs = append(page.IDs, id)
		}
	}
	return page, nil
}

// LinkIdentityProvider links person to the identity provider alias, as a
// first YTÜ login does in Keycloak (tests and local runs).
func (m *Memory) LinkIdentityProvider(alias string, person uuid.UUID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.idpLinks == nil {
		m.idpLinks = map[string]map[uuid.UUID]struct{}{}
	}
	if m.idpLinks[alias] == nil {
		m.idpLinks[alias] = map[uuid.UUID]struct{}{}
	}
	m.idpLinks[alias][person] = struct{}{}
}

// SetPersonalEmail gives person a Personal e-mail and its proof stamp ("" for
// none), the attributes sky-account writes (tests and local runs).
func (m *Memory) SetPersonalEmail(person uuid.UUID, address, verifiedAt string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.personalEmails == nil {
		m.personalEmails = map[uuid.UUID][2]string{}
	}
	m.personalEmails[person] = [2]string{address, verifiedAt}
}

// AddressHoldersPage pages through the people by id: every Primary e-mail
// verified (the memory directory keeps no flag), a username starting
// service-account- a service account.
func (m *Memory) AddressHoldersPage(_ context.Context, first, max int) (AddressHolderPage, error) {
	if first < 0 || max <= 0 {
		return AddressHolderPage{}, ErrInvalid
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.record("AddressHoldersPage")
	ids := make([]uuid.UUID, 0, len(m.people))
	for id := range m.people {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	if first > len(ids) {
		first = len(ids)
	}
	ids = ids[first:min(first+max, len(ids))]
	page := AddressHolderPage{Holders: make([]AddressHolder, 0, len(ids)), Rows: len(ids)}
	for _, id := range ids {
		p := m.answerLocked(m.people[id])
		personal := m.personalEmails[id]
		page.Holders = append(page.Holders, AddressHolder{
			ID: id, Enabled: p.Enabled, Email: p.Email, EmailVerified: true, SchoolEmail: p.SchoolEmail,
			PersonalEmail: personal[0], PersonalEmailVerifiedAt: personal[1],
			ServiceAccount: strings.HasPrefix(p.Username, serviceAccountPrefix),
		})
	}
	return page, nil
}

// IdentityProviderLinkedPage pages through the people LinkIdentityProvider
// linked to alias, by id.
func (m *Memory) IdentityProviderLinkedPage(_ context.Context, alias string, first, max int) (LinkedPage, error) {
	if first < 0 || max <= 0 || strings.TrimSpace(alias) == "" {
		return LinkedPage{}, ErrInvalid
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.record("IdentityProviderLinkedPage")
	ids := make([]uuid.UUID, 0, len(m.idpLinks[alias]))
	for id := range m.idpLinks[alias] {
		if _, ok := m.people[id]; ok {
			ids = append(ids, id)
		}
	}
	slices.SortFunc(ids, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	if first > len(ids) {
		first = len(ids)
	}
	ids = ids[first:min(first+max, len(ids))]
	return LinkedPage{IDs: ids, Rows: len(ids)}, nil
}
