package identity

import (
	"github.com/Nerzal/gocloak/v13"
	"github.com/google/uuid"
)

func groupFrom(g *gocloak.Group) Group {
	if g == nil {
		return Group{}
	}
	out := Group{}
	if g.ID != nil {
		out.ID = *g.ID
	}
	if g.Name != nil {
		out.Name = *g.Name
	}
	if g.Path != nil {
		out.Path = *g.Path
	}
	out.Attributes = firstAttrs(g.Attributes)
	return out
}

func personFrom(u *gocloak.User) (Person, error) {
	if u == nil || u.ID == nil {
		return Person{}, ErrInvalid
	}
	id, err := uuid.Parse(*u.ID)
	if err != nil {
		return Person{}, ErrInvalid
	}
	p := Person{ID: id}
	if u.Email != nil {
		p.Email = *u.Email
	}
	if u.FirstName != nil {
		p.FirstName = *u.FirstName
	}
	if u.LastName != nil {
		p.LastName = *u.LastName
	}
	if u.Username != nil {
		p.Username = *u.Username
	}
	return p, nil
}

func firstAttrs(raw *map[string][]string) map[string]string {
	if raw == nil {
		return nil
	}
	out := map[string]string{}
	for k, vals := range *raw {
		if len(vals) > 0 {
			out[k] = vals[0]
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func attrsToKC(attrs map[string]string) *map[string][]string {
	if attrs == nil {
		return nil
	}
	out := map[string][]string{}
	for k, v := range attrs {
		out[k] = []string{v}
	}
	return &out
}

func flattenGroups(gs []*gocloak.Group) []Group {
	out := make([]Group, 0)
	for _, g := range gs {
		if g == nil {
			continue
		}
		out = append(out, groupFrom(g))
		if g.SubGroups == nil {
			continue
		}
		subs := *g.SubGroups
		children := make([]*gocloak.Group, len(subs))
		for i := range subs {
			children[i] = &subs[i]
		}
		out = append(out, flattenGroups(children)...)
	}
	return out
}

func clientRolesFromMappings(m *gocloak.MappingsRepresentation) []ClientRole {
	if m == nil || m.ClientMappings == nil {
		return nil
	}
	out := make([]ClientRole, 0)
	for _, cm := range m.ClientMappings {
		if cm == nil || cm.Mappings == nil {
			continue
		}
		clientID := ""
		if cm.Client != nil {
			clientID = *cm.Client
		}
		for _, r := range *cm.Mappings {
			if r.Name == nil || *r.Name == "" {
				continue
			}
			out = append(out, ClientRole{ClientID: clientID, Role: *r.Name})
		}
	}
	return out
}

func dedupGroups(gs []Group) []Group {
	seen := make(map[string]struct{}, len(gs))
	out := make([]Group, 0, len(gs))
	for _, g := range gs {
		key := g.ID
		if key == "" {
			key = g.Path
		}
		if key != "" {
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
		}
		out = append(out, g)
	}
	return out
}
