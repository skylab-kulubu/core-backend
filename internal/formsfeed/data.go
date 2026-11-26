// Package formsfeed is what core does with Forms' word about its answers and
// forms (ADR-0071, docs/integration-events.md "core.forms"): the core.forms
// events, and the read of one answer from Forms' internal endpoint (claim
// check) when an event does not carry what a Ticket needs. Every way in ends
// in the Ticket rule the HTTP report uses (ticket.Service.RecordFormResponse),
// so a decision about an answer is the same whichever way it came.
//
// Nothing here writes a guest's name or e-mail to a log line, an error, a
// metric or a store: they live in memory for the length of one answer.
package formsfeed

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/google/uuid"
)

// Source is the CloudEvents source of Forms' events.
const Source = "/forms"

// Event types core reads from core.forms (ADR-0071 §2.5, ADR-0075 karar 5).
const (
	TypeResponseCreated       = "forms.response.created"
	TypeResponseStatusChanged = "forms.response.status_changed"
	TypeFormUpdated           = "forms.form.updated"
)

// SchemaVersion is the one data schema version core reads
// (urn:skylab:schema:<type>:1).
const SchemaVersion = 1

// InviteField is the hidden field whose value is a feedback invite's code
// (the ?davet= of the mail's link; etkinlik geri bildirim formu).
const InviteField = "davet"

// hiddenValue is a hidden field's value as an event may carry it (ADR-0071
// karar 5): a short code. Another value is left out.
var hiddenValue = regexp.MustCompile(`^[A-Za-z0-9_-]{1,100}$`)

// Statuses of an answer, as Forms reports them (CoreResponseFeed's mapping).
const (
	StatusAccepted = "accepted"
	StatusPending  = "pending"
	StatusDeclined = "declined"
)

// Guest is what a guest typed into the form's identity fields. Only the
// single answer read carries it, never an event.
type Guest struct {
	FirstName string `json:"firstName"`
	LastName  string `json:"lastName"`
	Email     string `json:"email"`
}

// Response is an answer as Forms reports it: forms.response.created's data,
// an item of the internal list (SPEC §10 eki), forms.response.status_changed's
// data (which lacks SubmittedAt, HasGuest and HiddenFields), or the single
// answer read (which adds Guest).
type Response struct {
	FormID     uuid.UUID
	ResponseID uuid.UUID
	Status     string
	UserID     *uuid.UUID
	HasGuest   bool
	// SubmittedAt is when Forms first took the answer; nil when not sent,
	// or not an RFC 3339 time with a zone (SubmittedAtIgnored then).
	SubmittedAt        *time.Time
	SubmittedAtIgnored bool
	// HiddenFields keeps the values that are short codes; another value,
	// or one that is not a string, is left out.
	HiddenFields map[string]string
	Version      int64
	Guest        *Guest
	// Answers are the answers to the questions core asked for (fields of
	// the internal reads, an Event's form mapping), by question id; nil
	// when it asked for none. A value that is not a string is left out.
	// They live in memory for the length of one answer, never logged or
	// kept.
	Answers map[string]string
}

// Invite is the feedback invite's code the answer came with, "" without one.
func (r Response) Invite() string { return r.HiddenFields[InviteField] }

type responseJSON struct {
	FormID       uuid.UUID                  `json:"formId"`
	ResponseID   uuid.UUID                  `json:"responseId"`
	Status       string                     `json:"status"`
	UserID       *uuid.UUID                 `json:"userId"`
	HasGuest     bool                       `json:"hasGuest"`
	SubmittedAt  json.RawMessage            `json:"submittedAt"`
	HiddenFields map[string]json.RawMessage `json:"hiddenFields"`
	Version      json.RawMessage            `json:"version"`
	Guest        *Guest                     `json:"guest"`
	Answers      map[string]json.RawMessage `json:"answers"`
}

// ErrInvalid is data core cannot read: a missing or malformed id, status or
// version. An event that carries it goes to the DLQ.
var ErrInvalid = errors.New("formsfeed: invalid data")

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// DecodeResponse reads an answer. The form and answer ids, the status and
// the version are required; a nil userId is a guest's (or nobody's) answer.
// A submittedAt that is not an RFC 3339 time with a zone is left out, as the
// HTTP report's is: the answer is not refused for it.
func DecodeResponse(raw json.RawMessage) (Response, error) {
	var in responseJSON
	if err := strictJSON(raw, &in); err != nil {
		return Response{}, err
	}
	if in.FormID == uuid.Nil || in.ResponseID == uuid.Nil {
		return Response{}, invalid("formId and responseId are required")
	}
	switch in.Status {
	case StatusAccepted, StatusPending, StatusDeclined:
	default:
		return Response{}, invalid("status is not accepted, pending or declined")
	}
	if in.UserID != nil && *in.UserID == uuid.Nil {
		return Response{}, invalid("userId is the nil UUID")
	}
	version, err := decodeVersion(in.Version)
	if err != nil {
		return Response{}, err
	}
	out := Response{
		FormID: in.FormID, ResponseID: in.ResponseID, Status: in.Status, UserID: in.UserID,
		HasGuest: in.HasGuest, Version: version, Guest: in.Guest,
	}
	out.SubmittedAt, out.SubmittedAtIgnored = decodeTime(in.SubmittedAt)
	for name, value := range in.HiddenFields {
		var text string
		if json.Unmarshal(value, &text) != nil || !hiddenValue.MatchString(text) {
			continue
		}
		if out.HiddenFields == nil {
			out.HiddenFields = map[string]string{}
		}
		out.HiddenFields[name] = text
	}
	for id, value := range in.Answers {
		var text string
		if json.Unmarshal(value, &text) != nil {
			continue
		}
		if out.Answers == nil {
			out.Answers = map[string]string{}
		}
		out.Answers[id] = text
	}
	return out, nil
}

// FormState is forms.form.updated's data: the form's state now.
type FormState struct {
	FormID uuid.UUID
	// Status is open, closed or deleted.
	Status string
	// ClosesAt is when an open form closes by itself; nil without a time.
	ClosesAt *time.Time
	Version  int64
}

// Form states.
const (
	FormOpen    = "open"
	FormClosed  = "closed"
	FormDeleted = "deleted"
)

type formStateJSON struct {
	FormID   uuid.UUID       `json:"formId"`
	Status   string          `json:"status"`
	ClosesAt json.RawMessage `json:"closesAt"`
	Version  json.RawMessage `json:"version"`
}

// DecodeFormState reads forms.form.updated's data. A closesAt that is not an
// RFC 3339 time with a zone is invalid: it is the whole of what the event
// says about an open form.
func DecodeFormState(raw json.RawMessage) (FormState, error) {
	var in formStateJSON
	if err := strictJSON(raw, &in); err != nil {
		return FormState{}, err
	}
	if in.FormID == uuid.Nil {
		return FormState{}, invalid("formId is required")
	}
	switch in.Status {
	case FormOpen, FormClosed, FormDeleted:
	default:
		return FormState{}, invalid("status is not open, closed or deleted")
	}
	version, err := decodeVersion(in.Version)
	if err != nil {
		return FormState{}, err
	}
	closesAt, ignored := decodeTime(in.ClosesAt)
	if ignored {
		return FormState{}, invalid("closesAt is not an RFC 3339 time")
	}
	return FormState{FormID: in.FormID, Status: in.Status, ClosesAt: closesAt, Version: version}, nil
}

func strictJSON(raw json.RawMessage, v any) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		return invalid("data is missing")
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return invalid("data is not the expected JSON object")
	}
	return nil
}

// decodeVersion reads a record's version (ADR-0071 §5: monotonic, the row
// version or UpdatedAt): a non-negative whole number, or an RFC 3339 time,
// taken as its microseconds since 1970. A publisher sends one form or the
// other, never both.
func decodeVersion(raw json.RawMessage) (int64, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return 0, invalid("version is required")
	}
	if raw[0] == '"' {
		var text string
		if err := json.Unmarshal(raw, &text); err == nil {
			if at, err := time.Parse(time.RFC3339Nano, text); err == nil {
				return at.UnixMicro(), nil
			}
		}
		return 0, invalid("version is neither a whole number nor an RFC 3339 time")
	}
	v, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil || v < 0 {
		return 0, invalid("version is neither a whole number nor an RFC 3339 time")
	}
	return v, nil
}

// decodeTime reads an optional RFC 3339 time with a zone; ignored when it is
// there but is not one.
func decodeTime(raw json.RawMessage) (at *time.Time, ignored bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, false
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		if t, err := time.Parse(time.RFC3339Nano, text); err == nil {
			t = t.UTC()
			return &t, false
		}
	}
	return nil, true
}
