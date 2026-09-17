package authz

import "testing"

func TestAuthorizer_Allow(t *testing.T) {
	t.Parallel()

	auth := NewAuthorizer(DefaultPolicy())

	tests := []struct {
		name string
		p    Principal
		r    Resource
		a    Action
		want bool
	}{
		{
			name: "event read is public",
			r:    Resource{Type: TypeEvent},
			a:    Read,
			want: true,
		},
		{
			name: "privileged YK can delete any event",
			p:    Principal{ID: "u1", Groups: []string{"/UYELER/YK"}},
			r:    Resource{Type: TypeEvent, OwnerTeam: "WEBLAB"},
			a:    Delete,
			want: true,
		},
		{
			name: "privileged YK subgroup can update",
			p:    Principal{ID: "u1", Groups: []string{"/UYELER/YK/BASKAN"}},
			r:    Resource{Type: TypeEvent, OwnerTeam: "SKYSEC"},
			a:    Update,
			want: true,
		},
		{
			name: "DK is privileged",
			p:    Principal{ID: "u1", Groups: []string{"/UYELER/DK"}},
			r:    Resource{Type: TypeEvent, OwnerTeam: "ARTLAB"},
			a:    Create,
			want: true,
		},
		{
			name: "owner team leader can update",
			p:    Principal{ID: "u1", Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}},
			r:    Resource{Type: TypeEvent, OwnerTeam: "WEBLAB"},
			a:    Update,
			want: true,
		},
		{
			name: "owner team member cannot delete",
			p:    Principal{ID: "u1", Groups: []string{"/UYELER/ARGE/WEBLAB"}},
			r:    Resource{Type: TypeEvent, OwnerTeam: "WEBLAB"},
			a:    Delete,
			want: false,
		},
		{
			name: "gecekodu member can update",
			p:    Principal{ID: "u1", Groups: []string{"/UYELER/ORGANIZASYON/GECEKODU"}},
			r:    Resource{Type: TypeEvent, OwnerTeam: "GECEKODU"},
			a:    Update,
			want: true,
		},
		{
			name: "gecekodu member cannot delete",
			p:    Principal{ID: "u1", Groups: []string{"/UYELER/ORGANIZASYON/GECEKODU"}},
			r:    Resource{Type: TypeEvent, OwnerTeam: "GECEKODU"},
			a:    Delete,
			want: false,
		},
		{
			name: "stranger cannot create",
			p:    Principal{ID: "u1", Groups: []string{"/UYELER/ARGE/WEBLAB"}},
			r:    Resource{Type: TypeEvent, OwnerTeam: "SKYSEC"},
			a:    Create,
			want: false,
		},
		{
			name: "empty owner is not a team fallback",
			p:    Principal{ID: "u1", Groups: []string{"/UYELER/ORGANIZASYON/GECEKODU/LIDERLER"}},
			r:    Resource{Type: TypeEvent},
			a:    Delete,
			want: false,
		},
		{
			name: "empty owner event is privileged-only",
			p:    Principal{ID: "u1", Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}},
			r:    Resource{Type: TypeEvent},
			a:    Create,
			want: false,
		},
		{
			name: "privileged can create event with no owner team",
			p:    Principal{ID: "u1", Groups: []string{"/UYELER/YK"}},
			r:    Resource{Type: TypeEvent},
			a:    Create,
			want: true,
		},
		{
			name: "privileged can list groups",
			p:    Principal{ID: "u1", Groups: []string{"/UYELER/YK"}},
			r:    Resource{Type: TypeGroup},
			a:    Read,
			want: true,
		},
		{
			name: "member cannot list groups",
			p:    Principal{ID: "u1", Groups: []string{"/UYELER/ARGE/WEBLAB"}},
			r:    Resource{Type: TypeGroup},
			a:    Read,
			want: false,
		},
		{
			name: "privileged can create users",
			p:    Principal{ID: "u1", Groups: []string{"/UYELER/ADMIN"}},
			r:    Resource{Type: TypeUser},
			a:    Create,
			want: true,
		},
		{
			name: "stranger cannot delete users",
			p:    Principal{ID: "u1", Groups: []string{"/UYELER/ARGE/WEBLAB"}},
			r:    Resource{Type: TypeUser},
			a:    Delete,
			want: false,
		},
		{
			name: "anonymous can read public teams",
			r:    Resource{Type: TypeTeam},
			a:    Read,
			want: true,
		},
		{
			name: "anonymous cannot mutate teams",
			r:    Resource{Type: TypeTeam},
			a:    Update,
			want: false,
		},
		{
			name: "authenticated can apply for a ticket",
			p:    Principal{ID: "u1"},
			r:    Resource{Type: TypeTicket},
			a:    Create,
			want: true,
		},
		{
			name: "anonymous cannot apply for a ticket",
			r:    Resource{Type: TypeTicket},
			a:    Create,
			want: false,
		},
		{
			name: "owner team leader can check in",
			p:    Principal{ID: "u1", Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}},
			r:    Resource{Type: TypeTicket, OwnerTeam: "WEBLAB"},
			a:    Validate,
			want: true,
		},
		{
			name: "owner team member cannot check in",
			p:    Principal{ID: "u1", Groups: []string{"/UYELER/ARGE/WEBLAB"}},
			r:    Resource{Type: TypeTicket, OwnerTeam: "WEBLAB"},
			a:    Validate,
			want: false,
		},
		{
			name: "privileged can check in",
			p:    Principal{ID: "u1", Groups: []string{"/UYELER/YK"}},
			r:    Resource{Type: TypeTicket, OwnerTeam: "WEBLAB"},
			a:    Validate,
			want: true,
		},
		{
			name: "leader can read event tickets",
			p:    Principal{ID: "u1", Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}},
			r:    Resource{Type: TypeTicket, OwnerTeam: "WEBLAB"},
			a:    Read,
			want: true,
		},
		{
			name: "member cannot read event tickets",
			p:    Principal{ID: "u1", Groups: []string{"/UYELER/ARGE/WEBLAB"}},
			r:    Resource{Type: TypeTicket, OwnerTeam: "WEBLAB"},
			a:    Read,
			want: false,
		},
		{
			name: "public can read seasons",
			r:    Resource{Type: TypeSeason},
			a:    Read,
			want: true,
		},
		{
			name: "member cannot create seasons",
			p:    Principal{ID: "u1", Groups: []string{"/UYELER/ARGE/WEBLAB"}},
			r:    Resource{Type: TypeSeason},
			a:    Create,
			want: false,
		},
		{
			name: "privileged can create seasons",
			p:    Principal{ID: "u1", Groups: []string{"/UYELER/YK"}},
			r:    Resource{Type: TypeSeason},
			a:    Create,
			want: true,
		},
		{
			name: "owner team leader can create event day",
			p:    Principal{ID: "u1", Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}},
			r:    Resource{Type: TypeEventDay, OwnerTeam: "WEBLAB"},
			a:    Create,
			want: true,
		},
		{
			name: "stranger cannot create session",
			p:    Principal{ID: "u1", Groups: []string{"/UYELER/ARGE/WEBLAB"}},
			r:    Resource{Type: TypeSession, OwnerTeam: "SKYSEC"},
			a:    Create,
			want: false,
		},
		{
			name: "anonymous cannot read competitors",
			r:    Resource{Type: TypeCompetitor, OwnerTeam: "WEBLAB"},
			a:    Read,
			want: false,
		},
		{
			name: "owner team can read event competitors",
			p:    Principal{ID: "u1", Groups: []string{"/UYELER/ARGE/WEBLAB"}},
			r:    Resource{Type: TypeCompetitor, OwnerTeam: "WEBLAB"},
			a:    Read,
			want: true,
		},
		{
			name: "member cannot dump all competitors",
			p:    Principal{ID: "u1", Groups: []string{"/UYELER/ARGE/WEBLAB"}},
			r:    Resource{Type: TypeCompetitor},
			a:    Read,
			want: false,
		},
		{
			name: "self can read own competitor row",
			p:    Principal{ID: "u1"},
			r:    Resource{Type: TypeCompetitor, OwnerID: "u1"},
			a:    Read,
			want: true,
		},
		{
			name: "authenticated can read own competitors",
			p:    Principal{ID: "u1"},
			r:    Resource{Type: TypeCompetitor},
			a:    ReadMe,
			want: true,
		},
		{
			name: "anonymous cannot create competitor",
			r:    Resource{Type: TypeCompetitor, OwnerTeam: "WEBLAB", OwnerID: "u1"},
			a:    Create,
			want: false,
		},
		{
			name: "self can register as competitor",
			p:    Principal{ID: "u1"},
			r:    Resource{Type: TypeCompetitor, OwnerTeam: "WEBLAB", OwnerID: "u1"},
			a:    Create,
			want: true,
		},
		{
			name: "stranger cannot register someone else",
			p:    Principal{ID: "u1", Groups: []string{"/UYELER/ARGE/SKYSEC"}},
			r:    Resource{Type: TypeCompetitor, OwnerTeam: "WEBLAB", OwnerID: "u2"},
			a:    Create,
			want: false,
		},
		{
			name: "owner team member can register someone else",
			p:    Principal{ID: "u1", Groups: []string{"/UYELER/ARGE/WEBLAB"}},
			r:    Resource{Type: TypeCompetitor, OwnerTeam: "WEBLAB", OwnerID: "u2"},
			a:    Create,
			want: true,
		},
		{
			name: "owner team member can update competitor scores",
			p:    Principal{ID: "u1", Groups: []string{"/UYELER/ARGE/WEBLAB"}},
			r:    Resource{Type: TypeCompetitor, OwnerTeam: "WEBLAB", OwnerID: "u2"},
			a:    Update,
			want: true,
		},
		{
			name: "self cannot update competitor scores",
			p:    Principal{ID: "u1"},
			r:    Resource{Type: TypeCompetitor, OwnerTeam: "WEBLAB", OwnerID: "u1"},
			a:    Update,
			want: false,
		},
		{
			name: "self can delete own competitor",
			p:    Principal{ID: "u1"},
			r:    Resource{Type: TypeCompetitor, OwnerTeam: "WEBLAB", OwnerID: "u1"},
			a:    Delete,
			want: true,
		},
		{
			name: "privileged can update any competitor",
			p:    Principal{ID: "u1", Groups: []string{"/UYELER/YK"}},
			r:    Resource{Type: TypeCompetitor, OwnerTeam: "WEBLAB", OwnerID: "u2"},
			a:    Update,
			want: true,
		},
		{
			name: "public can read media",
			r:    Resource{Type: TypeMedia},
			a:    Read,
			want: true,
		},
		{
			name: "authenticated can upload media",
			p:    Principal{ID: "u1"},
			r:    Resource{Type: TypeMedia},
			a:    Upload,
			want: true,
		},
		{
			name: "anonymous cannot upload media",
			r:    Resource{Type: TypeMedia},
			a:    Upload,
			want: false,
		},
		{
			name: "privileged can delete media",
			p:    Principal{ID: "u1", Groups: []string{"/UYELER/YK"}},
			r:    Resource{Type: TypeMedia},
			a:    Delete,
			want: true,
		},
		{
			name: "member cannot delete media",
			p:    Principal{ID: "u1", Groups: []string{"/UYELER/ARGE/WEBLAB"}},
			r:    Resource{Type: TypeMedia},
			a:    Delete,
			want: false,
		},
		{
			name: "url create with core access",
			p:    Principal{ID: "u1", Roles: []string{"url:access"}},
			r:    Resource{Type: TypeURL},
			a:    Create,
			want: true,
		},
		{
			name: "skylapp role does not grant url create",
			p:    Principal{ID: "u1", Roles: []string{"skylapp:access"}},
			r:    Resource{Type: TypeURL},
			a:    Create,
			want: false,
		},
		{
			name: "member cannot list all media",
			p:    Principal{ID: "u1", Groups: []string{"/UYELER/ARGE/WEBLAB"}},
			r:    Resource{Type: TypeMedia},
			a:    List,
			want: false,
		},
		{
			name: "privileged can list all media",
			p:    Principal{ID: "u1", Groups: []string{"/UYELER/YK"}},
			r:    Resource{Type: TypeMedia},
			a:    List,
			want: true,
		},
		{
			name: "url list all needs moderator",
			p:    Principal{ID: "u1", Roles: []string{"url:create"}},
			r:    Resource{Type: TypeURL},
			a:    Read,
			want: false,
		},
		{
			name: "url owner can update",
			p:    Principal{ID: "u1", Roles: []string{"url:update"}},
			r:    Resource{Type: TypeURL, OwnerID: "u1"},
			a:    Update,
			want: true,
		},
		{
			name: "owner team leader can issue certificate",
			p:    Principal{ID: "u1", Groups: []string{"/UYELER/ARGE/ARTLAB/LIDERLER"}},
			r:    Resource{Type: TypeCertificate, OwnerTeam: "ARTLAB"},
			a:    Issue,
			want: true,
		},
		{
			name: "owner team member cannot issue certificate",
			p:    Principal{ID: "u1", Groups: []string{"/UYELER/ARGE/ARTLAB"}},
			r:    Resource{Type: TypeCertificate, OwnerTeam: "ARTLAB"},
			a:    Issue,
			want: false,
		},
		{
			name: "gecekodu member cannot issue certificate",
			p:    Principal{ID: "u1", Groups: []string{"/UYELER/ORGANIZASYON/GECEKODU"}},
			r:    Resource{Type: TypeCertificate, OwnerTeam: "GECEKODU"},
			a:    Issue,
			want: false,
		},
		{
			name: "empty owner certificate is privileged-only",
			p:    Principal{ID: "u1", Groups: []string{"/UYELER/ARGE/ARTLAB/LIDERLER"}},
			r:    Resource{Type: TypeCertificate},
			a:    Issue,
			want: false,
		},
		{
			name: "privileged can issue empty owner certificate",
			p:    Principal{ID: "u1", Groups: []string{"/UYELER/YK"}},
			r:    Resource{Type: TypeCertificate},
			a:    Issue,
			want: true,
		},
		{
			name: "privileged can revoke certificate",
			p:    Principal{ID: "u1", Groups: []string{"/UYELER/YK"}},
			r:    Resource{Type: TypeCertificate, OwnerTeam: "ARTLAB"},
			a:    Revoke,
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := auth.Allow(tt.p, tt.r, tt.a)
			if got != tt.want {
				t.Fatalf("Allow() = %v, want %v", got, tt.want)
			}
		})
	}
}
