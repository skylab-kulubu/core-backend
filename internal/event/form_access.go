package event

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
)

// Refusals of linking a form the person writing the Event may not see in
// Forms (FormAccess).
var (
	// ErrFormsAccessDenied is a form the person is not on the team of in
	// Forms (owner, editor or viewer), one Forms does not have, or a
	// product's service account (403 forms_access_denied).
	ErrFormsAccessDenied = errors.New("event: not on the form's team in Forms")
	// ErrFormsUnavailable is a Forms core could not ask: no internal
	// address or service account, Forms down, or a refusal (503
	// forms_unavailable). Nothing is written.
	ErrFormsUnavailable = errors.New("event: Forms could not be read")
)

// FormAccess tells whether a person may see a form in Forms: its owner, an
// editor or a viewer there (Yusuf, 2026-11-26). Linking a form to an Event
// gathers every answer of it with core's own service account
// (docs/form-respondents.md), so a form the person cannot see in Forms is
// never linked. formsfeed.FormAccess implements it.
type FormAccess interface {
	// SeesForm answers nil when p may see formID in Forms,
	// ErrFormsAccessDenied when not, ErrFormsUnavailable when Forms could
	// not tell.
	SeesForm(ctx context.Context, p authz.Principal, formID uuid.UUID) error
}

// checkFormAccess asks Forms, for each application or extra form e lists
// that before did not (nil: every one, a new Event), whether p may see it
// there. A form the Event lists already is not asked about: a link made
// before the check stays, and a write that keeps it reads no Forms. Without
// a FormAccess (a test) nothing is asked.
func (s *service) checkFormAccess(ctx context.Context, p authz.Principal, e Event, before map[uuid.UUID]bool) error {
	if s.formAccess == nil {
		return nil
	}
	for _, id := range ApplicationFormIDs(e) {
		if before[id] {
			continue
		}
		if err := s.formAccess.SeesForm(ctx, p, id); err != nil {
			return err
		}
	}
	return nil
}
