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
