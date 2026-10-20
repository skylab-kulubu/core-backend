package authz

import "testing"

// Reading an Event's draft (ADR 0066, ReadFull on an Event): whoever may
// update the Event, delegation included, and a Privileged reader; no one
// else, a service account neither.
func TestEventDraftReaders(t *testing.T) {
	t.Parallel()
	weblab := Resource{Type: TypeEvent, OwnerTeam: "WEBLAB"}
	gecekodu := Resource{Type: TypeEvent, OwnerTeam: "GECEKODU"}
	for _, mode := range []RoleMode{RoleModeGroups, RoleModeBoth, RoleModeRoles} {
		az := NewAuthorizer(policyIn(mode))
		for _, tc := range []struct {
			name string
			p    Principal
			r    Resource
			want bool
		}{
			{"YK", ykMember(mode), weblab, true},
			{"DK", dkMember(mode), weblab, true},
			{"the team's Leader", Principal{ID: "l", Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}, weblab, true},
			{"another team's Leader", Principal{ID: "l", Groups: []string{"/UYELER/ARGE/ARTLAB/LIDERLER"}}, weblab, false},
			{"a member", Principal{ID: "m", Groups: []string{"/UYELER/ARGE/WEBLAB"}}, weblab, false},
			// GECEKODU's members may update its Events.
			{"a member who may update", Principal{ID: "m", Groups: []string{"/UYELER/GECEKODU"}}, gecekodu, true},
			{"signed out", Principal{}, weblab, false},
			{"a service account", Principal{ID: "s", ServiceAccount: true, Product: ProductForms, Roles: []string{"ticket:forms", RoleGlobalRead, RoleEventManage}}, weblab, false},
		} {
			if got := az.Allow(tc.p, tc.r, ReadFull); got != tc.want {
				t.Errorf("%s: %s = %v, want %v", mode, tc.name, got, tc.want)
			}
		}
		// DK reads a draft and still changes nothing.
		if az.Allow(dkMember(mode), weblab, Update) {
			t.Errorf("%s: DK updates an Event", mode)
		}
	}
}
