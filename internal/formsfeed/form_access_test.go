package formsfeed

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
)

// countingForm is stubForm that counts its reads.
type countingForm struct {
	stubForm
	reads int
}

func (c *countingForm) Form(ctx context.Context, id uuid.UUID) (Form, error) {
	c.reads++
	return c.stubForm.Form(ctx, id)
}

// Linking a form to an Event: the person writing it must be on the form's
// team in Forms, an owner, an editor or a viewer (Yusuf, 2026-11-26); a
// Forms that cannot tell is 503, never a link made unasked.
func TestFormAccessSeesForm(t *testing.T) {
	form := uuid.New()
	person := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
	team := func(role string) stubForm {
		return stubForm{form: Form{FormID: form, AccessListed: true, Collaborators: []Collaborator{
			{UserID: uuid.MustParse(person.ID), Role: role}, {UserID: uuid.New(), Role: RoleOwner}}}}
	}
	cases := []struct {
		name    string
		p       authz.Principal
		forms   FormReader
		want    error
		outcome string
		reads   bool
	}{
		{name: "the owner", p: person, forms: team(RoleOwner), outcome: `"outcome":"ok"`, reads: true},
		{name: "an editor", p: person, forms: team(RoleEditor), outcome: `"outcome":"ok"`, reads: true},
		{name: "a viewer", p: person, forms: team(RoleViewer), outcome: `"outcome":"ok"`, reads: true},
		{name: "not on the team", p: person, forms: stubForm{form: Form{FormID: form, AccessListed: true, Collaborators: []Collaborator{{UserID: uuid.New(), Role: RoleOwner}}}}, want: event.ErrFormsAccessDenied, outcome: `"outcome":"denied"`, reads: true},
		{name: "a Forms without the team", p: person, forms: stubForm{form: Form{FormID: form}}, want: event.ErrFormsAccessDenied, outcome: `"outcome":"denied"`, reads: true},
		{name: "a form Forms does not have", p: person, forms: stubForm{err: ErrNotFound}, want: event.ErrFormsAccessDenied, outcome: `"outcome":"denied"`, reads: true},
		{name: "a service account", p: authz.Principal{ID: person.ID, ServiceAccount: true}, forms: team(RoleOwner), want: event.ErrFormsAccessDenied, outcome: `"outcome":"denied"`},
		{name: "Forms refused core", p: person, forms: stubForm{err: ErrForbidden}, want: event.ErrFormsUnavailable, outcome: `"outcome":"forms_unavailable","reason":"forbidden"`, reads: true},
		{name: "Forms down", p: person, forms: stubForm{err: errors.New("formsfeed: Forms answered 503")}, want: event.ErrFormsUnavailable, outcome: `"outcome":"forms_unavailable","reason":"failed"`, reads: true},
		{name: "no Forms client", p: person, want: event.ErrFormsUnavailable, outcome: `"outcome":"forms_unavailable","reason":"no_client"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var lines []string
			access := &FormAccess{Logf: func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }}
			var counted *countingForm
			if tc.forms != nil {
				counted = &countingForm{stubForm: tc.forms.(stubForm)}
				access.Forms = counted
			}
			err := access.SeesForm(context.Background(), tc.p, form)
			if tc.want == nil && err != nil || tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("err %v, want %v", err, tc.want)
			}
			// The event package's errors only: the routes map those.
			if errors.Is(err, ErrFormsAccessDenied) {
				t.Fatalf("formsfeed's error leaked: %v", err)
			}
			if counted != nil && (counted.reads > 0) != tc.reads {
				t.Fatalf("reads %d, want reads %t", counted.reads, tc.reads)
			}
			if len(lines) != 1 || !strings.Contains(lines[0], `"event":"form_access_check"`) ||
				!strings.Contains(lines[0], form.String()) || !strings.Contains(lines[0], tc.outcome) || strings.Contains(lines[0], person.ID) {
				t.Fatalf("log %q", lines)
			}
		})
	}
	// A nil FormAccess is no client: 503, never a pass.
	var none *FormAccess
	if err := none.SeesForm(context.Background(), person, form); !errors.Is(err, event.ErrFormsUnavailable) {
		t.Fatalf("nil: %v", err)
	}
}
