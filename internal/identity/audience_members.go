package identity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/Nerzal/gocloak/v13"
	"github.com/google/uuid"
)

// AudiencePerson is a person as a News send's audience reads them
// (commpref.Audience, docs/news-audience.md): the Person, whether Keycloak
// has verified the Primary e-mail, and whether the account is a client's
// service account, never a person to write to.
type AudiencePerson struct {
	Person
	EmailVerified  bool
	ServiceAccount bool
}

// serviceAccountPrefix starts the username Keycloak gives a client's service
// account user.
const serviceAccountPrefix = "service-account-"

func audiencePersonFrom(u *gocloak.User) (AudiencePerson, error) {
	p, err := personFrom(u)
	if err != nil {
		return AudiencePerson{}, err
	}
	return AudiencePerson{
		Person:        p,
		EmailVerified: u.EmailVerified != nil && *u.EmailVerified,
		ServiceAccount: (u.ServiceAccountClientID != nil && *u.ServiceAccountClientID != "") ||
			strings.HasPrefix(p.Username, serviceAccountPrefix),
	}, nil
}

// audienceMembersPage is how many members one read of a Group asks for.
const audienceMembersPage = 200

// AudienceMembers lists the direct members of the Group groupID (not its
// subgroups') in full representation, page by page. An error names a
// status, never a person or the Group.
func (k *Keycloak) AudienceMembers(ctx context.Context, groupID string) ([]AudiencePerson, error) {
	token, err := k.accessToken(ctx)
	if err != nil {
		return nil, errors.New("identity: audience members read could not get an admin token")
	}
	var out []AudiencePerson
	for first := 0; ; first += audienceMembersPage {
		var page []json.RawMessage
		resp, err := k.gc.GetRequestWithBearerAuth(ctx, token).
			SetResult(&page).
			SetQueryParams(map[string]string{
				"first":               strconv.Itoa(first),
				"max":                 strconv.Itoa(audienceMembersPage),
				"briefRepresentation": "false",
			}).
			Get(k.adminBase + "/admin/realms/" + k.realm + "/groups/" + groupID + "/members")
		if err != nil {
			return nil, errors.New("identity: audience members read failed")
		}
		if resp.IsError() {
			if resp.StatusCode() == 404 {
				return nil, ErrNotFound
			}
			return nil, fmt.Errorf("identity: audience members read failed with status %d", resp.StatusCode())
		}
		for _, raw := range page {
			var u gocloak.User
			if err := json.Unmarshal(raw, &u); err != nil {
				continue
			}
			p, err := audiencePersonFrom(&u)
			if err != nil {
				continue
			}
			out = append(out, p)
		}
		if len(page) < audienceMembersPage {
			return out, nil
		}
	}
}

// AudiencePerson reads one person (a follower outside /UYELER). An error
// names a status, never the person.
func (k *Keycloak) AudiencePerson(ctx context.Context, id uuid.UUID) (AudiencePerson, error) {
	token, err := k.accessToken(ctx)
	if err != nil {
		return AudiencePerson{}, errors.New("identity: audience person read could not get an admin token")
	}
	u, err := k.gc.GetUserByID(ctx, token, k.realm, id.String())
	if err != nil {
		if mapKCErr(err) == ErrNotFound {
			return AudiencePerson{}, ErrNotFound
		}
		var api *gocloak.APIError
		if errors.As(err, &api) && api != nil && api.Code != 0 {
			return AudiencePerson{}, fmt.Errorf("identity: audience person read failed with status %d", api.Code)
		}
		return AudiencePerson{}, errors.New("identity: audience person read failed")
	}
	return audiencePersonFrom(u)
}

// AudienceMembers lists the direct members of the Group: every e-mail
// verified (the memory directory keeps no flag), a username starting
// service-account- a service account.
func (m *Memory) AudienceMembers(_ context.Context, groupID string) ([]AudiencePerson, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.record("AudienceMembers")
	g, ok := m.resolveLocked(groupID)
	if !ok {
		return nil, ErrNotFound
	}
	out := make([]AudiencePerson, 0, len(m.members[g.ID]))
	for id := range m.members[g.ID] {
		if p, ok := m.people[id]; ok {
			out = append(out, memoryAudiencePerson(m.answerLocked(p)))
		}
	}
	return out, nil
}

// AudiencePerson reads one person as AudienceMembers does.
func (m *Memory) AudiencePerson(_ context.Context, id uuid.UUID) (AudiencePerson, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.record("AudiencePerson")
	p, ok := m.people[id]
	if !ok {
		return AudiencePerson{}, ErrNotFound
	}
	return memoryAudiencePerson(m.answerLocked(p)), nil
}

func memoryAudiencePerson(p Person) AudiencePerson {
	return AudiencePerson{Person: p, EmailVerified: true, ServiceAccount: strings.HasPrefix(p.Username, serviceAccountPrefix)}
}
