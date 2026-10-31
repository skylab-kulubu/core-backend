// Package subjectrequest is the register of KVKK md. 11 data subject
// requests (docs/subject-requests.md): the board (YK) records each request
// that reaches info@, the post or a hand, and when and how it was answered,
// so the 30 days of KVKK md. 13 can be followed and proven. It keeps the
// least data that does that: the requester's name and where the answer
// went, never an ID number, a code or a document. Every change leaves a
// trail row with who, when and which fields, never a value. The periodic
// destruction run deletes a request three years after it was answered
// (internal/retention, subject_requests_age).
package subjectrequest

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/lifecycle"
	"github.com/skylab-kulubu/core-backend/internal/optional"
)

var (
	ErrNotFound  = errors.New("subjectrequest: not found")
	ErrForbidden = errors.New("subjectrequest: forbidden")
	ErrInvalid   = errors.New("subjectrequest: invalid")
	// ErrConflict: the change named a version (updatedAt) someone else has
	// changed since.
	ErrConflict = errors.New("subjectrequest: changed since read")
)

// InvalidError is a refused field, with the problem code the API answers.
type InvalidError struct{ Code string }

func (e *InvalidError) Error() string { return "subjectrequest: invalid: " + e.Code }

func (e *InvalidError) Is(target error) bool { return target == ErrInvalid }

func invalid(code string) error { return &InvalidError{Code: code} }

// Channel is how the request reached the club.
type Channel string

const (
	ChannelEmail    Channel = "email"
	ChannelPost     Channel = "post"
	ChannelInPerson Channel = "in_person"
)

// Type is what the request asks for: a clause of KVKK md. 11/1, or other
// (a consent withdrawal or a photo objection that came the same way).
type Type string

const (
	TypeLearn            Type = "learn"             // (a) whether their data is processed
	TypeInfo             Type = "info"              // (b) information on the processing
	TypePurpose          Type = "purpose"           // (c) its purpose and whether it is kept to
	TypeRecipients       Type = "recipients"        // (ç) the third parties it was transferred to
	TypeRectify          Type = "rectify"           // (d) rectification
	TypeErase            Type = "erase"             // (e) erasure or destruction (md. 7)
	TypeNotifyRecipients Type = "notify_recipients" // (f) telling the third parties of (d) and (e)
	TypeObjectAutomated  Type = "object_automated"  // (g) objection to an automated result
	TypeCompensation     Type = "compensation"      // (ğ) compensation of damage
	TypeOther            Type = "other"
)

// Types are every Type, in the law's order: a request's types are kept in it.
var Types = []Type{TypeLearn, TypeInfo, TypePurpose, TypeRecipients, TypeRectify, TypeErase,
	TypeNotifyRecipients, TypeObjectAutomated, TypeCompensation, TypeOther}

// Status is where the request stands. Answered and rejected are concluded.
type Status string

const (
	StatusReceived Status = "received"
	// StatusAwaitingVerification: a person without an account has not yet
	// confirmed the address is theirs (the procedure's one-time code).
	StatusAwaitingVerification Status = "awaiting_verification"
	StatusAnswered             Status = "answered"
	// StatusRejected: a reasoned refusal, an answer too.
	StatusRejected Status = "rejected"
)

// Concluded reports whether the request was answered or rejected.
func (s Status) Concluded() bool { return s == StatusAnswered || s == StatusRejected }

func (s Status) known() bool {
	return s == StatusReceived || s == StatusAwaitingVerification || s.Concluded()
}

// Action is what a trail row records.
type Action string

const (
	ActionCreate Action = "create"
	ActionUpdate Action = "update"
)

// DefaultResponder answers for the club (the data controller's representative).
const DefaultResponder = "YK Başkanı"

// DueDays is the answer's deadline (KVKK md. 13/2).
const DueDays = 30

// MaxAgeYears is how far back a request's day may lie: a mistyped year
// would otherwise date it, and its answer, early enough for the retention
// sweep to delete it.
const MaxAgeYears = 1

// DateLayout is how dates travel: a calendar day, no time.
const DateLayout = "2006-01-02"

// Limits of the free fields, in characters.
const (
	MaxNameLength      = 200
	MaxContactLength   = 320
	MaxSummaryLength   = 2000
	MaxResponderLength = 100
)

// ListLimit is the most requests a list answers.
const ListLimit = 500

// turkey is the club's day: "today" for the dates' checks.
var turkey = time.FixedZone("TRT", 3*60*60)

// Request is one data subject request.
type Request struct {
	ID               uuid.UUID  `json:"id"`
	ReceivedOn       string     `json:"receivedOn"`
	Channel          Channel    `json:"channel"`
	RequesterName    string     `json:"requesterName"`
	RequesterContact string     `json:"requesterContact"`
	UserID           *uuid.UUID `json:"userId"`
	RequestTypes     []Type     `json:"requestTypes"`
	Status           Status     `json:"status"`
	DueOn            string     `json:"dueOn"`
	AnsweredOn       *string    `json:"answeredOn"`
	AnswerSummary    string     `json:"answerSummary"`
	Responder        string     `json:"responder"`
	CreatedAt        time.Time  `json:"createdAt"`
	CreatedBy        *uuid.UUID `json:"createdBy"`
	UpdatedAt        time.Time  `json:"updatedAt"`
	UpdatedBy        *uuid.UUID `json:"updatedBy"`
	// Events is the trail, oldest first: only on a single request.
	Events []Event `json:"events,omitempty"`
}

// Event is one row of the trail: no value, only the fields' names.
type Event struct {
	At      time.Time  `json:"at"`
	ActorID *uuid.UUID `json:"actorId"`
	Action  Action     `json:"action"`
	Fields  []string   `json:"fields"`
	Status  Status     `json:"status"`
}

// Input is a new request (POST /v1/subject-requests). An empty status is
// received; an empty responder is DefaultResponder.
type Input struct {
	ReceivedOn       string  `json:"receivedOn"`
	Channel          Channel `json:"channel"`
	RequesterName    string  `json:"requesterName"`
	RequesterContact string  `json:"requesterContact"`
	// UserID is the account's id as text: anything but one is user_invalid.
	UserID        *string `json:"userId"`
	RequestTypes  []Type  `json:"requestTypes"`
	Status        Status  `json:"status"`
	AnsweredOn    *string `json:"answeredOn"`
	AnswerSummary string  `json:"answerSummary"`
	Responder     string  `json:"responder"`
}

// Patch is a change (PATCH /v1/subject-requests/{id}): a field left out
// keeps its value; null clears the account and the answer's date and
// empties the summary and the responder. Moving the request back to an open
// status clears the answer's date unless the patch sends one (and is
// refused then).
type Patch struct {
	ReceivedOn       optional.Field[string]  `json:"receivedOn"`
	Channel          optional.Field[Channel] `json:"channel"`
	RequesterName    optional.Field[string]  `json:"requesterName"`
	RequesterContact optional.Field[string]  `json:"requesterContact"`
	UserID           optional.Field[*string] `json:"userId"`
	RequestTypes     optional.Field[[]Type]  `json:"requestTypes"`
	Status           optional.Field[Status]  `json:"status"`
	AnsweredOn       optional.Field[*string] `json:"answeredOn"`
	AnswerSummary    optional.Field[string]  `json:"answerSummary"`
	Responder        optional.Field[string]  `json:"responder"`
	// UpdatedAt, when sent, is the version the change was made on: the
	// request's updatedAt as last read. Someone else's change since refuses
	// it (ErrConflict). Left out, the change is written as before.
	UpdatedAt *time.Time `json:"updatedAt"`
}

// Store keeps the register. Create and Update write the request and its
// trail row together; DueOn is the store's (received + DueDays).
type Store interface {
	List(ctx context.Context, limit int) ([]Request, error)
	Get(ctx context.Context, id uuid.UUID) (Request, error)
	Events(ctx context.Context, id uuid.UUID) ([]Event, error)
	Create(ctx context.Context, r Request, e Event) (Request, error)
	Update(ctx context.Context, r Request, e Event) (Request, error)
}

// Service is the register as the API uses it. Every call is the board's
// alone (authz.TypeSubjectRequest): a refusal comes before any read.
type Service interface {
	List(ctx context.Context, p authz.Principal) ([]Request, error)
	Get(ctx context.Context, p authz.Principal, id uuid.UUID) (Request, error)
	Create(ctx context.Context, p authz.Principal, in Input) (Request, error)
	Update(ctx context.Context, p authz.Principal, id uuid.UUID, patch Patch) (Request, error)
}

type service struct {
	store Store
	authz authz.Authorizer
	now   func() time.Time
}

// NewService decides with az and dates with now (time.Now when nil).
func NewService(store Store, az authz.Authorizer, now func() time.Time) Service {
	if now == nil {
		now = time.Now
	}
	return &service{store: store, authz: az, now: now}
}

func (s *service) allow(p authz.Principal, action authz.Action) error {
	if !s.authz.Allow(p, authz.Resource{Type: authz.TypeSubjectRequest}, action) {
		return ErrForbidden
	}
	return nil
}

func (s *service) List(ctx context.Context, p authz.Principal) ([]Request, error) {
	if err := s.allow(p, authz.List); err != nil {
		return nil, err
	}
	return s.store.List(ctx, ListLimit)
}

func (s *service) Get(ctx context.Context, p authz.Principal, id uuid.UUID) (Request, error) {
	if err := s.allow(p, authz.Read); err != nil {
		return Request{}, err
	}
	r, err := s.store.Get(ctx, id)
	if err != nil {
		return Request{}, err
	}
	if r.Events, err = s.store.Events(ctx, id); err != nil {
		return Request{}, err
	}
	return r, nil
}

func (s *service) Create(ctx context.Context, p authz.Principal, in Input) (Request, error) {
	if err := s.allow(p, authz.Create); err != nil {
		return Request{}, err
	}
	userID, err := parseUserID(in.UserID)
	if err != nil {
		return Request{}, err
	}
	r := Request{
		ID: uuid.New(), ReceivedOn: in.ReceivedOn, Channel: in.Channel, RequesterName: in.RequesterName,
		RequesterContact: in.RequesterContact, UserID: userID, RequestTypes: in.RequestTypes, Status: in.Status,
		AnsweredOn: in.AnsweredOn, AnswerSummary: in.AnswerSummary, Responder: in.Responder,
	}
	if r.Status == "" {
		r.Status = StatusReceived
	}
	if r, err = s.normalize(r, true); err != nil {
		return Request{}, err
	}
	now := s.stamp()
	actor := lifecycle.ActorID(p.ID)
	r.CreatedAt, r.CreatedBy, r.UpdatedAt, r.UpdatedBy = now, actor, now, actor
	return s.store.Create(ctx, r, Event{At: now, ActorID: actor, Action: ActionCreate, Fields: changed(Request{}, r), Status: r.Status})
}

func (s *service) Update(ctx context.Context, p authz.Principal, id uuid.UUID, patch Patch) (Request, error) {
	if err := s.allow(p, authz.Update); err != nil {
		return Request{}, err
	}
	existing, err := s.store.Get(ctx, id)
	if err != nil {
		return Request{}, err
	}
	if patch.UpdatedAt != nil && !patch.UpdatedAt.Equal(existing.UpdatedAt) {
		return Request{}, ErrConflict
	}
	next, err := patch.apply(existing)
	if err != nil {
		return Request{}, err
	}
	// The year's bound is on the day as entered: an old request stays
	// open to change (closing it, mainly) once its day is past it.
	if next, err = s.normalize(next, next.ReceivedOn != existing.ReceivedOn); err != nil {
		return Request{}, err
	}
	fields := changed(existing, next)
	if len(fields) == 0 {
		return existing, nil
	}
	now := s.stamp()
	actor := lifecycle.ActorID(p.ID)
	next.UpdatedAt, next.UpdatedBy = now, actor
	return s.store.Update(ctx, next, Event{At: now, ActorID: actor, Action: ActionUpdate, Fields: fields, Status: next.Status})
}

// stamp is now as the database keeps it (microseconds), so a version read
// back compares equal to the one written.
func (s *service) stamp() time.Time { return s.now().Truncate(time.Microsecond) }

// parseUserID reads an account id: nil stays nil, anything but a UUID other
// than the nil one is user_invalid.
func parseUserID(raw *string) (*uuid.UUID, error) {
	if raw == nil {
		return nil, nil
	}
	id, err := uuid.Parse(strings.TrimSpace(*raw))
	if err != nil || id == uuid.Nil {
		return nil, invalid("user_invalid")
	}
	return &id, nil
}

func (patch Patch) apply(r Request) (Request, error) {
	out := r
	notNull := func(null bool, code string) error {
		if null {
			return invalid(code)
		}
		return nil
	}
	for _, check := range []error{
		notNull(patch.ReceivedOn.Null, "received_on_invalid"),
		notNull(patch.Channel.Null, "channel_invalid"),
		notNull(patch.RequesterName.Null, "requester_name_invalid"),
		notNull(patch.RequesterContact.Null, "requester_contact_invalid"),
		notNull(patch.RequestTypes.Null, "request_types_invalid"),
		notNull(patch.Status.Null, "status_invalid"),
	} {
		if check != nil {
			return Request{}, check
		}
	}
	out.ReceivedOn = patch.ReceivedOn.Or(r.ReceivedOn)
	out.Channel = patch.Channel.Or(r.Channel)
	out.RequesterName = patch.RequesterName.Or(r.RequesterName)
	out.RequesterContact = patch.RequesterContact.Or(r.RequesterContact)
	if patch.UserID.Set {
		userID, err := parseUserID(patch.UserID.Value)
		if err != nil {
			return Request{}, err
		}
		out.UserID = userID
	}
	out.RequestTypes = patch.RequestTypes.Or(r.RequestTypes)
	out.Status = patch.Status.Or(r.Status)
	out.AnsweredOn = patch.AnsweredOn.Or(r.AnsweredOn)
	out.AnswerSummary = patch.AnswerSummary.Or(r.AnswerSummary)
	out.Responder = patch.Responder.Or(r.Responder)
	if !out.Status.Concluded() && !patch.AnsweredOn.Set {
		out.AnsweredOn = nil
	}
	return out, nil
}

// normalize trims the free fields, keeps the types once each in the law's
// order and checks everything the table checks, with "today" in Turkey;
// entered, the received day may lie at most MaxAgeYears back.
func (s *service) normalize(r Request, entered bool) (Request, error) {
	today := s.now().In(turkey).Format(DateLayout)
	r.RequesterName = strings.TrimSpace(r.RequesterName)
	r.RequesterContact = strings.TrimSpace(r.RequesterContact)
	r.AnswerSummary = strings.TrimSpace(r.AnswerSummary)
	r.Responder = strings.TrimSpace(r.Responder)
	if r.Responder == "" {
		r.Responder = DefaultResponder
	}
	received, ok := parseDate(r.ReceivedOn)
	earliest := s.now().In(turkey).AddDate(-MaxAgeYears, 0, 0).Format(DateLayout)
	switch {
	case !ok || r.ReceivedOn > today || (entered && r.ReceivedOn < earliest):
		return Request{}, invalid("received_on_invalid")
	case r.Channel != ChannelEmail && r.Channel != ChannelPost && r.Channel != ChannelInPerson:
		return Request{}, invalid("channel_invalid")
	case r.RequesterName == "" || utf8.RuneCountInString(r.RequesterName) > MaxNameLength:
		return Request{}, invalid("requester_name_invalid")
	case r.RequesterContact == "" || utf8.RuneCountInString(r.RequesterContact) > MaxContactLength:
		return Request{}, invalid("requester_contact_invalid")
	case !r.Status.known():
		return Request{}, invalid("status_invalid")
	case utf8.RuneCountInString(r.AnswerSummary) > MaxSummaryLength:
		return Request{}, invalid("answer_summary_invalid")
	case utf8.RuneCountInString(r.Responder) > MaxResponderLength:
		return Request{}, invalid("responder_invalid")
	}
	types := make([]Type, 0, len(r.RequestTypes))
	for _, t := range Types {
		if slices.Contains(r.RequestTypes, t) {
			types = append(types, t)
		}
	}
	for _, t := range r.RequestTypes {
		if !slices.Contains(Types, t) {
			return Request{}, invalid("request_types_invalid")
		}
	}
	if len(types) == 0 {
		return Request{}, invalid("request_types_invalid")
	}
	r.RequestTypes = types
	if r.Status.Concluded() && r.AnsweredOn == nil {
		return Request{}, invalid("answered_on_required")
	}
	if r.AnsweredOn != nil {
		answered, ok := parseDate(*r.AnsweredOn)
		if !ok || !r.Status.Concluded() || answered.Before(received) || *r.AnsweredOn > today {
			return Request{}, invalid("answered_on_invalid")
		}
	}
	return r, nil
}

func parseDate(s string) (time.Time, bool) {
	t, err := time.Parse(DateLayout, s)
	return t, err == nil && t.Format(DateLayout) == s
}

// changed names the fields whose values differ, in the API's order: the
// trail's only content.
func changed(before, after Request) []string {
	var out []string
	add := func(name string, differ bool) {
		if differ {
			out = append(out, name)
		}
	}
	add("receivedOn", before.ReceivedOn != after.ReceivedOn)
	add("channel", before.Channel != after.Channel)
	add("requesterName", before.RequesterName != after.RequesterName)
	add("requesterContact", before.RequesterContact != after.RequesterContact)
	add("userId", !equalPtr(before.UserID, after.UserID))
	add("requestTypes", !slices.Equal(before.RequestTypes, after.RequestTypes))
	add("status", before.Status != after.Status)
	add("answeredOn", !equalPtr(before.AnsweredOn, after.AnsweredOn))
	add("answerSummary", before.AnswerSummary != after.AnswerSummary)
	add("responder", before.Responder != after.Responder)
	return out
}

func equalPtr[T comparable](a, b *T) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// DueOn is the deadline of a request received on received (a valid date).
func DueOn(received string) string {
	t, _ := time.Parse(DateLayout, received)
	return t.AddDate(0, 0, DueDays).Format(DateLayout)
}
