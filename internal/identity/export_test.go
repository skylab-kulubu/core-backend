package identity

import "github.com/Nerzal/gocloak/v13"

func FlattenGroupsForTest(gs []*gocloak.Group) []Group {
	return flattenGroups(gs)
}

func PersonFromForTest(u *gocloak.User) (Person, error) {
	return personFrom(u)
}

func ClientRolesFromMappingsForTest(m *gocloak.MappingsRepresentation) []ClientRole {
	return clientRolesFromMappings(m)
}
