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
			r:    Resource{Type: TypeEvent, OwnerTeam: "WEBLAB", EventType: "AGC"},
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
			r:    Resource{Type: TypeEvent, OwnerTeam: "WEBLAB", EventType: "AGC"},
			a:    Update,
			want: true,
		},
		{
			name: "owner team member cannot delete default event type",
			p:    Principal{ID: "u1", Groups: []string{"/UYELER/ARGE/WEBLAB"}},
			r:    Resource{Type: TypeEvent, OwnerTeam: "WEBLAB", EventType: "AGC"},
			a:    Delete,
			want: false,
		},
		{
			name: "gecekodu member can update",
			p:    Principal{ID: "u1", Groups: []string{"/UYELER/ORGANIZASYON/GECEKODU"}},
			r:    Resource{Type: TypeEvent, OwnerTeam: "GECEKODU", EventType: "GECEKODU"},
			a:    Update,
			want: true,
		},
		{
			name: "gecekodu member cannot delete",
			p:    Principal{ID: "u1", Groups: []string{"/UYELER/ORGANIZASYON/GECEKODU"}},
			r:    Resource{Type: TypeEvent, OwnerTeam: "GECEKODU", EventType: "GECEKODU"},
			a:    Delete,
			want: false,
		},
		{
			name: "stranger cannot create",
			p:    Principal{ID: "u1", Groups: []string{"/UYELER/ARGE/WEBLAB"}},
			r:    Resource{Type: TypeEvent, OwnerTeam: "SKYSEC", EventType: "AGC"},
			a:    Create,
			want: false,
		},
		{
			name: "owner falls back to event type",
			p:    Principal{ID: "u1", Groups: []string{"/UYELER/ORGANIZASYON/GECEKODU/LIDERLER"}},
			r:    Resource{Type: TypeEvent, EventType: "GECEKODU"},
			a:    Delete,
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
			r:    Resource{Type: TypeEventDay, OwnerTeam: "WEBLAB", EventType: "WEBLAB"},
			a:    Create,
			want: true,
		},
		{
			name: "stranger cannot create session",
			p:    Principal{ID: "u1", Groups: []string{"/UYELER/ARGE/WEBLAB"}},
			r:    Resource{Type: TypeSession, OwnerTeam: "SKYSEC", EventType: "SKYSEC"},
			a:    Create,
			want: false,
		},
		{
			name: "public can read competitors",
			r:    Resource{Type: TypeCompetitor, OwnerTeam: "WEBLAB"},
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
