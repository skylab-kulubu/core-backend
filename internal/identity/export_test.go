package identity

import (
	"context"
	"time"

	"github.com/Nerzal/gocloak/v13"
)

func FlattenGroupsForTest(gs []*gocloak.Group) []Group {
	return flattenGroups(gs)
}

func PersonFromForTest(u *gocloak.User) (Person, error) {
	return personFrom(u)
}

func ClientRolesFromMappingsForTest(m *gocloak.MappingsRepresentation) []ClientRole {
	return clientRolesFromMappings(m)
}

func (k *Keycloak) TimeoutForTest() time.Duration {
	return k.timeout
}

func (k *Keycloak) SetLoginForTest(login func(context.Context) (*gocloak.JWT, error)) {
	k.login = login
}
