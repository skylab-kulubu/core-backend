// Package feedback keeps the feedback invites of the Events' and Sessions'
// feedback forms (etkinlik geri bildirim formu, ticket 04;
// docs/event-feedback.md): one row per Ticket and target whose form was
// mailed, with the Ticket's own code, so that the reminder goes only to whom
// has not answered. Core keeps no answer: Forms does.
package feedback

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

var (
	// ErrExists refuses a second invite for the same Ticket and target.
	ErrExists = errors.New("feedback: invite exists")
	// ErrInvalid refuses an invite without a Ticket, an Event or a form.
	ErrInvalid = errors.New("feedback: invalid")
)

// InviteParam is the form address's query parameter that carries the code.
const InviteParam = "davet"

// MaxReportedInvite is the longest invite a forms report may carry; a
// longer one is ignored, as an unknown one is.
const MaxReportedInvite = 64

// Invite is one Ticket's invite to one target's feedback form: the Event's
// (SessionID nil) or one of its Sessions'.
type Invite struct {
	ID                  uuid.UUID
	TicketID            uuid.UUID
	EventID             uuid.UUID
	SessionID           *uuid.UUID
	FormID              uuid.UUID
	Code                string
	SentAt              time.Time
	RemindedAt          *time.Time
	AnsweredAt          *time.Time
	ReminderCancelledAt *time.Time
}

// Count is how many invites a target's form went out in, and how many of
// them were answered: what stays once the invites are deleted.
type Count struct {
	SessionID *uuid.UUID `json:"sessionId,omitempty"`
	Sent      int        `json:"sent"`
	Answered  int        `json:"answered"`
}

// Counts are an Event's counts: its own form's (none when it was never
// mailed) and each Session's form's.
type Counts struct {
	Event    Count   `json:"event"`
	Sessions []Count `json:"sessions"`
}

var codePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{22}$`)

// NewCode is a new invite code: 128 random bits, base64url, 22 characters.
func NewCode() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

// ValidCode reports whether code has an invite code's form.
func ValidCode(code string) bool {
	return codePattern.MatchString(code)
}

// Link is the invite's link: the form's own address (never the short link,
// whose clicks are recorded with their query) with the code alone as its
// query. Nothing else goes into it: not the Sessions the person attended
// (decision S1).
func Link(formURL, code string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(formURL))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", ErrInvalid
	}
	parsed.RawQuery = url.Values{InviteParam: {code}}.Encode()
	parsed.Fragment = ""
	return parsed.String(), nil
}
