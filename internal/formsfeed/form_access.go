package formsfeed

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
)

// seesForm reads the form from Forms and checks p is on its team there
// (Form.Sees: owner, editor or viewer). ErrFormsAccessDenied when not: a
// product's service account never is, nor anyone of a form Forms does not
// have. ErrFormsUnavailable (with the cause) when Forms could not tell: no
// client, Forms down, a refusal of core, an answer core cannot read.
func seesForm(ctx context.Context, forms FormReader, p authz.Principal, formID uuid.UUID) error {
	if p.ServiceAccount {
		return ErrFormsAccessDenied
	}
	if forms == nil {
		return ErrFormsUnavailable
	}
	form, err := forms.Form(ctx, formID)
	switch {
	case errors.Is(err, ErrNotFound):
		return ErrFormsAccessDenied
	case err != nil:
		return errors.Join(ErrFormsUnavailable, err)
	}
	if !form.Sees(p.ID) {
		return ErrFormsAccessDenied
	}
	return nil
}

// FormAccess is event.FormAccess on Forms' form read: an Event lists a form
// newly only when the person writing it is on the form's team in Forms
// (docs/form-respondents.md#who-may-link-a-form).
type FormAccess struct {
	// Forms nil is a core without Forms' internal address or its service
	// account: every check is event.ErrFormsUnavailable.
	Forms FormReader
	// Logf writes one line a check (the form and how it ended, never who
	// asked); nil writes none.
	Logf func(string, ...any)
}

// SeesForm implements event.FormAccess.
func (a *FormAccess) SeesForm(ctx context.Context, p authz.Principal, formID uuid.UUID) error {
	var forms FormReader
	if a != nil {
		forms = a.Forms
	}
	err := seesForm(ctx, forms, p, formID)
	outcome, reason := "ok", ""
	switch {
	case errors.Is(err, ErrFormsUnavailable):
		outcome, reason = "forms_unavailable", "failed"
		switch {
		case forms == nil:
			reason = "no_client"
		case errors.Is(err, ErrForbidden):
			reason = "forbidden"
		case errors.Is(err, ErrInvalid):
			reason = "invalid"
		}
		err = errors.Join(event.ErrFormsUnavailable, err)
	case err != nil:
		outcome, err = "denied", event.ErrFormsAccessDenied
	}
	if a != nil && a.Logf != nil {
		if line, marshalErr := json.Marshal(struct {
			Event   string `json:"event"`
			FormID  string `json:"form_id"`
			Outcome string `json:"outcome"`
			Reason  string `json:"reason,omitempty"`
		}{"form_access_check", formID.String(), outcome, reason}); marshalErr == nil {
			a.Logf("%s", line)
		}
	}
	return err
}
