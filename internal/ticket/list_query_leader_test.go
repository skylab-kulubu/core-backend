package ticket_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

// A caller with no visible ticket and no Ticket read gets 200 [] only as a
// Leader: the whole last segment must be a leader subgroup's.
func TestService_ListQueryLeaderNeedsWholeSegment(t *testing.T) {
	t.Parallel()
	cases := []struct {
		group  string
		leader bool
	}{
		{"/UYELER/ARGE/WEBLAB/LIDERLER", true},
		{"/UYELER/ARGE/WEBLAB/KOORDINATORLER", true},
		{"/UYELER/ARGE/WEBLAB/LIDERLER-ESKI", false},
		{"/UYELER/ARGE/WEBLAB/LIDERLERX", false},
		{"/UYELER/ARGE/WEBLAB/KOORDINATORLER-ESKI", false},
		{"/UYELER/ARGE/WEBLAB/LIDERLER/X", false},
		{"/UYELER/ARGE/WEBLAB", false},
	}
	for _, tc := range cases {
		t.Run(tc.group, func(t *testing.T) {
			t.Parallel()
			_, svc := setup(t)
			other := uuid.New()
			p := authz.Principal{ID: "caller", Groups: []string{tc.group}}
			got, err := svc.ListQuery(context.Background(), p, "", &other)
			if tc.leader {
				if err != nil || len(got) != 0 {
					t.Fatalf("got %v, %v; want empty list", got, err)
				}
				return
			}
			if !errors.Is(err, ticket.ErrForbidden) {
				t.Fatalf("got %v, %v; want ErrForbidden", got, err)
			}
		})
	}
}
