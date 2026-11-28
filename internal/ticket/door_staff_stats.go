package ticket

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// The door staff statistics (docs/door-staff-stats.md): for an Event, each
// person who wrote at its door, by name, with what they wrote. Who may
// read the Event's Tickets reads every row; who may only take check-ins at
// its door reads their own row.

// The scopes of the statistics.
const (
	DoorStatsAll  = "all"
	DoorStatsSelf = "self"
)

// DoorEntryCounts are Venue entries a person wrote, by source.
type DoorEntryCounts struct {
	Scan   int `json:"scan"`
	Desk   int `json:"desk"`
	WalkIn int `json:"walkIn"`
	Total  int `json:"total"`
}

// DoorCheckInCounts are Session Check-ins a person wrote, by source.
type DoorCheckInCounts struct {
	Scan  int `json:"scan"`
	Desk  int `json:"desk"`
	Total int `json:"total"`
}

// DoorHour is the marks a person wrote in one Istanbul hour: Hour is its
// start.
type DoorHour struct {
	Hour  time.Time `json:"hour"`
	Count int       `json:"count"`
}

// DoorStaffRow is one person's work at the Event's door. Entries and
// CheckIns count what stands (a mark taken back is in Undone); WalkIns the
// Tickets they wrote at the door ("Kapıda kayıt"); Undone their marks
// taken back since the door kept them (2026-11-28), Undid the marks they
// took back. FirstAt and LastAt span their marks and Walk-ins; Hours their
// marks (entries and Check-ins) by hour.
type DoorStaffRow struct {
	UserID   uuid.UUID         `json:"userId"`
	Name     string            `json:"name"`
	Entries  DoorEntryCounts   `json:"entries"`
	CheckIns DoorCheckInCounts `json:"checkIns"`
	WalkIns  int               `json:"walkIns"`
	Undone   int               `json:"undone"`
	Undid    int               `json:"undid"`
	FirstAt  *time.Time        `json:"firstAt"`
	LastAt   *time.Time        `json:"lastAt"`
	Hours    []DoorHour        `json:"hours"`
}

// DoorStatsOthers are the Event's marks no door staff is named on: the
// people's own (the attendance QR and the like), and the door's whose
// writer is not known (a Check-in before the door kept it, an erased
// account, a service).
type DoorStatsOthers struct {
	SelfEntries          int `json:"selfEntries"`
	SelfCheckIns         int `json:"selfCheckIns"`
	UnattributedEntries  int `json:"unattributedEntries"`
	UnattributedCheckIns int `json:"unattributedCheckIns"`
}

// DoorStaffStats is GET /v1/events/{id}/door-staff/stats.
type DoorStaffStats struct {
	Scope  string           `json:"scope"`
	Staff  []DoorStaffRow   `json:"staff"`
	Others *DoorStatsOthers `json:"others"`
}

// DoorMarkCount is the store's count of one writer's marks of one kind and
// source (By nil: nobody named), with their first and last moment.
type DoorMarkCount struct {
	By      *uuid.UUID
	Kind    string
	Source  string
	Count   int
	FirstAt time.Time
	LastAt  time.Time
}

// DoorPersonCount is a count of one person's (a Walk-in's writer, an
// undo's writer or taker) with its first and last moment.
type DoorPersonCount struct {
	UserID  uuid.UUID
	Count   int
	FirstAt time.Time
	LastAt  time.Time
}

// DoorStatsData is what the store reads for the statistics.
type DoorStatsData struct {
	Marks   []DoorMarkCount
	Hours   map[uuid.UUID][]DoorHour
	WalkIns []DoorPersonCount
	Undone  []DoorPersonCount
	Undid   []DoorPersonCount
}

// DoorStatsStore reads the statistics (PostgresStore).
type DoorStatsStore interface {
	DoorStats(ctx context.Context, eventID uuid.UUID) (DoorStatsData, error)
}

// DoorStaffStats is the Event's door staff statistics for p: every row for
// a reader of its Tickets (the Event's editors, its Co-organizers, DK and
// the Privileged, as the export), p's own row for its door staff; else
// ErrForbidden. A draft p may not see is ErrNotFound.
func (s *service) DoorStaffStats(ctx context.Context, p authz.Principal, eventID uuid.UUID) (DoorStaffStats, error) {
	ev, err := s.eventOf(ctx, eventID)
	if err != nil {
		return DoorStaffStats{}, err
	}
	if err := s.seen(&p, ev); err != nil {
		return DoorStaffStats{}, err
	}
	scope := DoorStatsAll
	if !s.readsEventTickets(p, ev) {
		if !s.canUseDoor(ctx, p, ev) {
			return DoorStaffStats{}, ErrForbidden
		}
		scope = DoorStatsSelf
	}
	return s.doorStaffStats(ctx, ev, scope, p)
}

// readsEventTickets reports whether p reads ev's Tickets: a person (no
// service) the Ticket read allows, as the export asks.
func (s *service) readsEventTickets(p authz.Principal, ev event.Event) bool {
	return p.ID != "" && p.Product == "" && s.authz.Allow(p, eventResource(ev, authz.TypeTicket), authz.Read)
}

func (s *service) doorStaffStats(ctx context.Context, ev event.Event, scope string, p authz.Principal) (DoorStaffStats, error) {
	store, ok := s.tickets.(DoorStatsStore)
	if !ok {
		return DoorStaffStats{}, ErrStatusUnavailable
	}
	data, err := store.DoorStats(ctx, ev.ID)
	if err != nil {
		return DoorStaffStats{}, err
	}
	out := DoorStaffStats{Scope: scope, Staff: []DoorStaffRow{}}
	rows := map[uuid.UUID]*DoorStaffRow{}
	row := func(id uuid.UUID) *DoorStaffRow {
		r, ok := rows[id]
		if !ok {
			r = &DoorStaffRow{UserID: id, Hours: []DoorHour{}}
			rows[id] = r
		}
		return r
	}
	span := func(r *DoorStaffRow, first, last time.Time) {
		if r.FirstAt == nil || first.Before(*r.FirstAt) {
			f := first
			r.FirstAt = &f
		}
		if r.LastAt == nil || last.After(*r.LastAt) {
			l := last
			r.LastAt = &l
		}
	}
	others := DoorStatsOthers{}
	for _, m := range data.Marks {
		switch {
		case m.Kind == KindEntry && m.Source == string(EntryAttendance):
			others.SelfEntries += m.Count
			continue
		case m.Kind == KindCheckIn && m.Source == string(CheckInSelf):
			others.SelfCheckIns += m.Count
			continue
		case m.By == nil && m.Kind == KindEntry:
			others.UnattributedEntries += m.Count
			continue
		case m.By == nil:
			others.UnattributedCheckIns += m.Count
			continue
		}
		r := row(*m.By)
		span(r, m.FirstAt, m.LastAt)
		if m.Kind == KindEntry {
			switch EntrySource(m.Source) {
			case EntryScan:
				r.Entries.Scan += m.Count
			case EntryDesk:
				r.Entries.Desk += m.Count
			case EntryWalkIn:
				r.Entries.WalkIn += m.Count
			}
			r.Entries.Total += m.Count
			continue
		}
		if CheckInSource(m.Source) == CheckInScan {
			r.CheckIns.Scan += m.Count
		} else {
			// desk, or a Check-in before the door kept its source whose
			// writer its Venue entry names (a desk's or none).
			r.CheckIns.Desk += m.Count
		}
		r.CheckIns.Total += m.Count
	}
	for _, w := range data.WalkIns {
		r := row(w.UserID)
		r.WalkIns += w.Count
		span(r, w.FirstAt, w.LastAt)
	}
	for _, u := range data.Undone {
		row(u.UserID).Undone += u.Count
	}
	for _, u := range data.Undid {
		row(u.UserID).Undid += u.Count
	}
	for id, hours := range data.Hours {
		if r, ok := rows[id]; ok {
			r.Hours = hours
		}
	}
	if scope == DoorStatsSelf {
		self, err := uuid.Parse(p.ID)
		if err != nil {
			return out, nil
		}
		for id := range rows {
			if id != self {
				delete(rows, id)
			}
		}
	} else {
		out.Others = &others
	}
	if err := s.nameDoorStaff(ctx, rows); err != nil {
		return DoorStaffStats{}, err
	}
	for _, r := range rows {
		out.Staff = append(out.Staff, *r)
	}
	slices.SortFunc(out.Staff, func(a, b DoorStaffRow) int {
		if x, y := doorMarks(a), doorMarks(b); x != y {
			return y - x
		}
		if c := strings.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		return strings.Compare(a.UserID.String(), b.UserID.String())
	})
	return out, nil
}

// doorMarks is what a row wrote at the door: its marks and Walk-ins.
func doorMarks(r DoorStaffRow) int {
	return r.Entries.Total + r.CheckIns.Total + r.WalkIns
}

// nameDoorStaff names each row by the account's name (never its e-mail);
// an erased or blocked account is user.DeletedDisplayName.
func (s *service) nameDoorStaff(ctx context.Context, rows map[uuid.UUID]*DoorStaffRow) error {
	if len(rows) == 0 || s.users == nil {
		return nil
	}
	ids := make([]uuid.UUID, 0, len(rows))
	for id := range rows {
		ids = append(ids, id)
	}
	accounts, err := s.users.Accounts(ctx, ids)
	if err != nil {
		return err
	}
	for id, r := range rows {
		a, ok := accounts[id]
		if !ok || a.Blocked || !a.Stored {
			r.Name = user.DeletedDisplayName
			continue
		}
		// No e-mail nor username in its place: the panel shows a
		// nameless account as such.
		r.Name = strings.TrimSpace(a.User.FirstName + " " + a.User.LastName)
	}
	return nil
}
