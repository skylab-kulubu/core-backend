package event

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/optional"
)

// Patch is the body of PUT and PATCH /v1/events/{id}: a field left out
// keeps the Event's value, null (or "" for text) clears a field that may be
// empty (ADR 0066 addendum, ticket 13). Name, location and the Owner team
// may not be cleared, nor ranked or registrationClosed set null: that is
// ErrInvalid. The body's id, status and active are not read: the status
// changes only through its endpoints, and active is derived.
type Patch struct {
	Name            optional.Field[string]          `json:"name"`
	Description     optional.Field[string]          `json:"description"`
	Location        optional.Field[string]          `json:"location"`
	OwnerTeam       optional.Field[string]          `json:"ownerTeam"`
	FormURL         optional.Field[string]          `json:"formUrl"`
	FormAlias       optional.Field[string]          `json:"formAlias"`
	ExtraFormURLs   optional.Field[[]EventFormLink] `json:"extraFormUrls"`
	Capacity        optional.Field[int]             `json:"capacity"`
	StartDate       optional.Field[*time.Time]      `json:"startDate"`
	EndDate         optional.Field[*time.Time]      `json:"endDate"`
	Linkedin        optional.Field[string]          `json:"linkedin"`
	Ranked          optional.Field[bool]            `json:"ranked"`
	PrizeInfo       optional.Field[string]          `json:"prizeInfo"`
	CoverImageID    optional.Field[*uuid.UUID]      `json:"coverImageId"`
	AttendanceRule  optional.Field[string]          `json:"attendanceRule"`
	AttendanceRatio optional.Field[*float64]        `json:"attendanceRatio"`
	DoorStaffIDs    optional.Field[[]uuid.UUID]     `json:"doorStaffIds"`
	// RegistrationClosed closes or opens applying by hand (null is
	// ErrInvalid); RegistrationClosesAt sets the deadline, null clears it
	// (ticket 12).
	RegistrationClosed   optional.Field[bool]       `json:"registrationClosed"`
	RegistrationClosesAt optional.Field[*time.Time] `json:"registrationClosesAt"`
	// CancelReason edits a cancelled Event's public reason (null or ""
	// clears it). On an Event that is not cancelled it is ignored, as the
	// status is: the write applies it only while the Event is cancelled.
	CancelReason optional.Field[string] `json:"cancelReason"`
	// FeedbackMode, FeedbackFormURL, FeedbackFormAlias and FeedbackAutoSend
	// are the Event's feedback (etkinlik geri bildirim formu, ticket 02):
	// the mode and the box may not be null; the form and its alias are
	// cleared with null or "".
	FeedbackMode      optional.Field[FeedbackMode] `json:"feedbackMode"`
	FeedbackFormURL   optional.Field[string]       `json:"feedbackFormUrl"`
	FeedbackFormAlias optional.Field[string]       `json:"feedbackFormAlias"`
	FeedbackAutoSend  optional.Field[bool]         `json:"feedbackAutoSend"`
}

// required is the value of a field that may not be emptied: sent, it must
// hold a non-blank value.
func required(f optional.Field[string], kept string) (string, error) {
	if f.Set && strings.TrimSpace(f.Value) == "" {
		return "", ErrInvalid
	}
	return f.Or(kept), nil
}

// apply writes the fields the body sent over existing.
func (p Patch) apply(existing Event) (Event, error) {
	e := existing
	var err error
	if e.Name, err = required(p.Name, existing.Name); err != nil {
		return Event{}, err
	}
	if e.Location, err = required(p.Location, existing.Location); err != nil {
		return Event{}, err
	}
	if e.OwnerTeam, err = required(p.OwnerTeam, existing.OwnerTeam); err != nil {
		return Event{}, err
	}
	if p.Ranked.Null || p.RegistrationClosed.Null || p.FeedbackMode.Null || p.FeedbackAutoSend.Null {
		return Event{}, ErrInvalid
	}
	if e.FeedbackMode = p.FeedbackMode.Or(existing.FeedbackMode); !e.FeedbackMode.Valid() {
		return Event{}, ErrInvalid
	}
	e.FeedbackFormURL = strings.TrimSpace(p.FeedbackFormURL.Or(existing.FeedbackFormURL))
	e.FeedbackFormAlias = strings.TrimSpace(p.FeedbackFormAlias.Or(existing.FeedbackFormAlias))
	e.FeedbackAutoSend = p.FeedbackAutoSend.Or(existing.FeedbackAutoSend)
	e.Description = p.Description.Or(existing.Description)
	e.FormURL = p.FormURL.Or(existing.FormURL)
	e.FormAlias = p.FormAlias.Or(existing.FormAlias)
	if p.ExtraFormURLs.Set {
		e.ExtraFormURLs = append([]EventFormLink{}, p.ExtraFormURLs.Value...)
	}
	e.Capacity = p.Capacity.Or(existing.Capacity)
	e.StartDate = p.StartDate.Or(existing.StartDate)
	e.EndDate = p.EndDate.Or(existing.EndDate)
	e.Linkedin = p.Linkedin.Or(existing.Linkedin)
	e.Ranked = p.Ranked.Or(existing.Ranked)
	e.PrizeInfo = p.PrizeInfo.Or(existing.PrizeInfo)
	e.CoverImageID = p.CoverImageID.Or(existing.CoverImageID)
	e.AttendanceRule = p.AttendanceRule.Or(existing.AttendanceRule)
	e.AttendanceRatio = p.AttendanceRatio.Or(existing.AttendanceRatio)
	if p.DoorStaffIDs.Set {
		e.DoorStaffIDs = append([]uuid.UUID{}, p.DoorStaffIDs.Value...)
	}
	e.RegistrationClosed = p.RegistrationClosed.Or(existing.RegistrationClosed)
	e.RegistrationClosesAt = p.RegistrationClosesAt.Or(existing.RegistrationClosesAt)
	if p.CancelReason.Set {
		reason := strings.TrimSpace(p.CancelReason.Value)
		if utf8.RuneCountInString(reason) > CancelReasonMax {
			return Event{}, ErrInvalid
		}
		e.CancelReason, e.cancelReasonSent = reason, true
	}
	return e, nil
}

// touchesFormLinks reports whether the body changes what the Event's form
// short links are built from: its forms, their aliases, or the name that
// labels them. A body that sends none of them leaves the links alone.
func (p Patch) touchesFormLinks() bool {
	return p.FormURL.Set || p.FormAlias.Set || p.ExtraFormURLs.Set || p.Name.Set ||
		p.FeedbackFormURL.Set || p.FeedbackFormAlias.Set
}

// Patch updates the fields the body sent and keeps the others.
func (s *service) Patch(ctx context.Context, p authz.Principal, id uuid.UUID, patch Patch) (Event, error) {
	return s.update(ctx, p, id, patch.apply, patch.touchesFormLinks())
}
