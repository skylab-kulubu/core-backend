package ticket

// InvalidError is an ErrInvalid that says why, as the problem code the API
// answers (docs/problem-codes.md): the client tells the person what to fix
// instead of a bare 400. Fields are the problem's extension members (the
// missing fields, a limit).
type InvalidError struct {
	Code   string
	Fields map[string]any
}

func (e *InvalidError) Error() string { return "ticket: invalid: " + e.Code }

func (e *InvalidError) Is(target error) bool { return target == ErrInvalid }

func invalid(code string) error { return &InvalidError{Code: code} }

func invalidWith(code string, fields map[string]any) error {
	return &InvalidError{Code: code, Fields: fields}
}

// The codes of InvalidError (docs/problem-codes.md, "Biletler ve kapı").
const (
	CodeGuestFieldsRequired      = "guest_fields_required"
	CodeEmailOrTicketKeyRequired = "email_or_ticket_key_required"
	CodeDoorQueryRequired        = "door_query_required"
	CodeEntryDateInvalid         = "entry_date_invalid"
	CodeSessionNotInEvent        = "session_not_in_event"
	CodeCancelReasonTooLong      = "cancel_reason_too_long"
	CodeRequestKeyTooLong        = "request_key_too_long"
	CodeTicketQueryRequired      = "ticket_query_required"
	CodeFormIDRequired           = "form_id_required"
	CodeFormStatusInvalid        = "form_status_invalid"
	CodeUserIDInvalid            = "user_id_invalid"
	CodeGuestNameRequired        = "guest_name_required"
	CodeTokenSubjectInvalid      = "token_subject_invalid"
	// CodeDoorQRStaticOff: the static attendance QR was asked for while
	// DOOR_QR_STATIC=off.
	CodeDoorQRStaticOff = "door_qr_static_off"
	// CodeSessionTimesRequired: a static attendance QR for a Session without
	// a start or an end time.
	CodeSessionTimesRequired = "session_times_required"
)
