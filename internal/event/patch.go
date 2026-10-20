package event

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/optional"
)

// Patch is the body of PUT and PATCH /v1/events/{id}: a field left out
// keeps the Event's value, null (or "" for text) clears a field that may be
// empty (ADR 0066 addendum, ticket 13). Name, location and the Owner team
// may not be cleared, nor ranked set null: that is ErrInvalid. The body's
// id, status and active are not read: the status changes only through its
// endpoints, and active is derived.
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
	if p.Ranked.Null {
		return Event{}, ErrInvalid
	}
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
	return e, nil
}

// touchesFormLinks reports whether the body changes what the Event's form
// short links are built from: its forms, their aliases, or the name that
// labels them. A body that sends none of them leaves the links alone.
func (p Patch) touchesFormLinks() bool {
	return p.FormURL.Set || p.FormAlias.Set || p.ExtraFormURLs.Set || p.Name.Set
}

// Patch updates the fields the body sent and keeps the others.
func (s *service) Patch(ctx context.Context, p authz.Principal, id uuid.UUID, patch Patch) (Event, error) {
	return s.update(ctx, p, id, patch.apply, patch.touchesFormLinks())
}
