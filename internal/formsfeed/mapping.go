package formsfeed

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Mapping is an Event's form mapping (docs/form-mappings.md): which
// questions of a form the Event lists hold a guest's first name, last name,
// full name ("ad soyad" in one question) and e-mail. Core builds a guest
// Ticket from those answers when Forms' report of an answer names nobody
// (no userId, no named guest). The optional details (phone, university,
// department, grade) go on a guest's Ticket, a named guest's too. Only the
// question ids are kept, never an answer.
type Mapping struct {
	EventID uuid.UUID
	FormID  uuid.UUID
	// FirstName, LastName, FullName and Email are question ids; "" is none.
	FirstName string
	LastName  string
	FullName  string
	Email     string
	// Phone, University, Department and Grade are the optional details'
	// question ids; "" is none.
	Phone      string
	University string
	Department string
	Grade      string
	// UpdatedBy is who saved the mapping last (a person's id), UpdatedAt
	// when.
	UpdatedBy *uuid.UUID
	UpdatedAt time.Time
}

// ErrMappingInvalid is a mapping that breaks the rule: an e-mail question,
// and either a first and a last name question or a full name question, each
// (and each detail's, all optional) a question id of at most MaxFieldLength
// characters without a comma, none named twice.
var ErrMappingInvalid = errors.New("formsfeed: the form mapping needs an e-mail question and a first and last name, or a full name, question")

// Normalized trims the question ids.
func (m Mapping) Normalized() Mapping {
	m.FirstName = strings.TrimSpace(m.FirstName)
	m.LastName = strings.TrimSpace(m.LastName)
	m.FullName = strings.TrimSpace(m.FullName)
	m.Email = strings.TrimSpace(m.Email)
	m.Phone = strings.TrimSpace(m.Phone)
	m.University = strings.TrimSpace(m.University)
	m.Department = strings.TrimSpace(m.Department)
	m.Grade = strings.TrimSpace(m.Grade)
	return m
}

// Validate checks the rule of ErrMappingInvalid on the normalized mapping.
func (m Mapping) Validate() error {
	m = m.Normalized()
	if m.Email == "" || ((m.FirstName == "" || m.LastName == "") && m.FullName == "") {
		return ErrMappingInvalid
	}
	seen := map[string]bool{}
	for _, id := range m.Fields() {
		if len(id) > MaxFieldLength || strings.Contains(id, ",") || seen[id] {
			return ErrMappingInvalid
		}
		seen[id] = true
	}
	return nil
}

// Fields are the mapping's question ids, in the order first name, last
// name, full name, e-mail, then the details': what core asks Forms for
// (fields).
func (m Mapping) Fields() []string {
	var out []string
	for _, id := range []string{m.FirstName, m.LastName, m.FullName, m.Email} {
		if id != "" {
			out = append(out, id)
		}
	}
	return append(out, m.DetailFields()...)
}

// DetailFields are the question ids of the optional details, in the order
// phone, university, department, grade.
func (m Mapping) DetailFields() []string {
	var out []string
	for _, id := range []string{m.Phone, m.University, m.Department, m.Grade} {
		if id != "" {
			out = append(out, id)
		}
	}
	return out
}

// HasDetails reports whether the mapping names a detail's question.
func (m Mapping) HasDetails() bool { return len(m.DetailFields()) > 0 }

// MissingFrom are the mapping's question ids the form does not have.
func (m Mapping) MissingFrom(questions []Question) []string {
	have := map[string]bool{}
	for _, q := range questions {
		have[q.ID] = true
	}
	var missing []string
	for _, id := range m.Fields() {
		if !have[id] {
			missing = append(missing, id)
		}
	}
	return missing
}

// MappingQuestionMissingError is a mapping naming questions the form does
// not have (422 mapping_question_missing): Missing are their ids, which the
// caller sent.
type MappingQuestionMissingError struct{ Missing []string }

func (e *MappingQuestionMissingError) Error() string {
	return fmt.Sprintf("formsfeed: the form has no question of %d of the mapping's ids", len(e.Missing))
}

// Suggest is the mapping a form's identity fields give (SPEC §11 K5,
// decided by Yusuf on 2026-11-25 as proposed): a form with identity fields
// (props.identity firstName, lastName, email) already makes Forms send a
// named guest, so the panel opens its mapping filled from them, and the
// organizer may change it. A field more than one question claims is left
// empty. This is the one place K5 is decided in core; the panel's guesses
// from the input type and the question's text are the panel's (SPEC §5.2).
func Suggest(questions []Question) Mapping {
	found := map[string]string{}
	count := map[string]int{}
	for _, q := range questions {
		switch q.Identity {
		case "firstName", "lastName", "email":
			found[q.Identity] = q.ID
			count[q.Identity]++
		}
	}
	pick := func(field string) string {
		if count[field] == 1 {
			return found[field]
		}
		return ""
	}
	return Mapping{FirstName: pick("firstName"), LastName: pick("lastName"), Email: pick("email")}
}
