// Package dashboard answers the admin panel's summary in one request: the
// counts and per-day figures the panel used to compute by reading every
// Event and then each Event's Tickets (docs/dashboard-summary.md).
//
// It decides nothing about permissions itself. Which Events a caller's
// summary covers is the Ticket read decision core already makes for an
// Event's applicant list (authz TypeTicket, Read, by Owner team), and the
// Members section is the User read decision of the user list (authz
// TypeUser, Read).
package dashboard

import (
	"context"
	"slices"
	"sort"
	"time"
	// The summary counts days in the club's time zone whatever the host's
	// zone database holds.
	_ "time/tzdata"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
)

const (
	// TimeZone is the zone the summary's days and months are counted in:
	// the club's.
	TimeZone = "Europe/Istanbul"
	// DailyDays is how many days the applications per day cover, today
	// included.
	DailyDays = 30
	// EventDailyDays is how many days an Event's own applications per day
	// cover, today included.
	EventDailyDays = 14
	// RecentEventDays is how long after it ended an Event stays in
	// EventStats.
	RecentEventDays = 30
	// EventMonths is how many months Events.ByMonth covers, this one
	// included.
	EventMonths = 6
	// undatedEventSpan is how long an Event without an end date counts as
	// running from its start, as the panel counts it.
	undatedEventSpan = 12 * time.Hour
)

// Summary is the answer of GET /v1/dashboard/summary.
type Summary struct {
	GeneratedAt time.Time `json:"generatedAt"`
	// TimeZone is the zone the days and months below are counted in.
	TimeZone string `json:"timeZone"`
	// OwnerTeams are the Owner teams whose Events the summary covers.
	OwnerTeams   []string     `json:"ownerTeams"`
	Events       EventCounts  `json:"events"`
	Applications Applications `json:"applications"`
	// EventStats are the covered Events that are running, still to come,
	// or ended within RecentEventDays, by start date.
	EventStats []EventStat `json:"eventStats"`
	// Members is nil for a caller who may not read people.
	Members *Members `json:"members"`
	// MembersUnavailable is true when the caller may read people but the
	// directory could not be read; the rest of the summary still stands.
	MembersUnavailable bool `json:"membersUnavailable,omitempty"`
}

// EventCounts count the current (not archived) Events the summary covers.
type EventCounts struct {
	Total int `json:"total"`
	// Upcoming have not started yet.
	Upcoming int `json:"upcoming"`
	// Live are running now.
	Live int `json:"live"`
	// ByMonth counts them by the month they start in, EventMonths months
	// up to this one, oldest first.
	ByMonth []MonthCount `json:"byMonth"`
}

// Applications count the Tickets of every Event the summary covers.
type Applications struct {
	Total     int `json:"total"`
	Members   int `json:"members"`
	Guests    int `json:"guests"`
	CheckedIn int `json:"checkedIn"`
	// Daily counts them by the day they came in, DailyDays days up to
	// today, oldest first.
	Daily []DayCount `json:"daily"`
}

type DayCount struct {
	// Date is the day in TimeZone, YYYY-MM-DD.
	Date  string `json:"date"`
	Count int    `json:"count"`
}

type MonthCount struct {
	// Month is the month in TimeZone, YYYY-MM.
	Month string `json:"month"`
	Count int    `json:"count"`
}

// EventStat is one Event with its applications.
type EventStat struct {
	ID            uuid.UUID  `json:"id"`
	Name          string     `json:"name"`
	OwnerTeam     string     `json:"ownerTeam"`
	Location      string     `json:"location"`
	StartDate     *time.Time `json:"startDate,omitempty"`
	EndDate       *time.Time `json:"endDate,omitempty"`
	Active        bool       `json:"active"`
	Live          bool       `json:"live"`
	Capacity      int        `json:"capacity"`
	CoverImageURL string     `json:"coverImageUrl,omitempty"`
	// HasApplicationForm is false when the Event links no form, main or
	// extra.
	HasApplicationForm bool `json:"hasApplicationForm"`
	HasCoverImage      bool `json:"hasCoverImage"`
	Applications       int  `json:"applications"`
	Members            int  `json:"members"`
	Guests             int  `json:"guests"`
	CheckedIn          int  `json:"checkedIn"`
	// DailyApplications are the Event's applications per day,
	// EventDailyDays days up to today, oldest first.
	DailyApplications []int `json:"dailyApplications"`
}

// EventLister reads the current (not archived) Events: event.Store.
type EventLister interface {
	List(ctx context.Context, ownerTeam string, activeOnly bool) ([]event.Event, error)
}

type Service interface {
	Summary(ctx context.Context, p authz.Principal) (Summary, error)
}

type Options struct {
	Events EventLister
	Store  Store
	Authz  authz.Authorizer
	// Directory reads the Members tree. Nil leaves the Members section
	// out (null) for everyone.
	Directory GroupDirectory
	// MembersTTL is how long one read of the Members tree is used.
	// Zero is DefaultMembersTTL.
	MembersTTL time.Duration
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

type service struct {
	events  EventLister
	store   Store
	authz   authz.Authorizer
	members *memberCache
	now     func() time.Time
	loc     *time.Location
}

func NewService(o Options) (Service, error) {
	loc, err := time.LoadLocation(TimeZone)
	if err != nil {
		return nil, err
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	s := &service{events: o.Events, store: o.Store, authz: o.Authz, now: now, loc: loc}
	if o.Directory != nil {
		ttl := o.MembersTTL
		if ttl <= 0 {
			ttl = DefaultMembersTTL
		}
		s.members = newMemberCache(o.Directory, ttl, now)
	}
	return s, nil
}

func (s *service) Summary(ctx context.Context, p authz.Principal) (Summary, error) {
	now := s.now()
	all, err := s.events.List(ctx, "", false)
	if err != nil {
		return Summary{}, err
	}
	covered := s.covered(p, all)
	ids := make([]uuid.UUID, len(covered))
	teams := make([]string, 0)
	for i, e := range covered {
		ids[i] = e.ID
		if e.OwnerTeam != "" && !slices.Contains(teams, e.OwnerTeam) {
			teams = append(teams, e.OwnerTeam)
		}
	}
	sort.Strings(teams)

	counts := map[uuid.UUID]TicketCounts{}
	daily := map[uuid.UUID]map[string]int{}
	today := startOfDay(now, s.loc)
	days := dayKeys(today, DailyDays)
	if len(ids) > 0 {
		if counts, err = s.store.TicketCounts(ctx, ids); err != nil {
			return Summary{}, err
		}
		since := today.AddDate(0, 0, -(DailyDays - 1))
		if daily, err = s.store.DailyApplications(ctx, ids, since, s.loc); err != nil {
			return Summary{}, err
		}
	}

	out := Summary{
		GeneratedAt: now.UTC(),
		TimeZone:    TimeZone,
		OwnerTeams:  teams,
		Events:      s.eventCounts(covered, now),
		Applications: Applications{
			Daily: make([]DayCount, len(days)),
		},
		EventStats: make([]EventStat, 0),
	}
	for i, day := range days {
		out.Applications.Daily[i].Date = day
		for _, byDay := range daily {
			out.Applications.Daily[i].Count += byDay[day]
		}
	}
	for _, c := range counts {
		out.Applications.Total += c.Applications
		out.Applications.Members += c.Members
		out.Applications.Guests += c.Guests
		out.Applications.CheckedIn += c.CheckedIn
	}
	eventDays := days[len(days)-EventDailyDays:]
	recent := now.Add(-RecentEventDays * 24 * time.Hour)
	for _, e := range covered {
		start, end, dated := span(e)
		if !dated || end.Before(recent) {
			continue
		}
		c := counts[e.ID]
		stat := EventStat{
			ID: e.ID, Name: e.Name, OwnerTeam: e.OwnerTeam, Location: e.Location,
			StartDate: e.StartDate, EndDate: e.EndDate, Active: e.Active,
			Live:     !start.After(now) && !now.After(end),
			Capacity: e.Capacity, CoverImageURL: e.Resource().CoverImageURL,
			HasApplicationForm: e.FormURL != "" || len(e.ExtraFormURLs) > 0,
			HasCoverImage:      e.CoverImageID != nil || e.CoverImageURL != "",
			Applications:       c.Applications, Members: c.Members, Guests: c.Guests, CheckedIn: c.CheckedIn,
			DailyApplications: make([]int, len(eventDays)),
		}
		for i, day := range eventDays {
			stat.DailyApplications[i] = daily[e.ID][day]
		}
		out.EventStats = append(out.EventStats, stat)
	}
	sort.SliceStable(out.EventStats, func(i, j int) bool {
		return out.EventStats[i].StartDate.Before(*out.EventStats[j].StartDate)
	})

	if s.members != nil && s.authz.Allow(p, authz.Resource{Type: authz.TypeUser}, authz.Read) {
		members, err := s.memberSummary(ctx, now)
		if err != nil {
			out.MembersUnavailable = true
		} else {
			out.Members = &members
		}
	}
	return out, nil
}

// covered are the Events whose applications p may read: the decision of
// GET /v1/events/{id}/tickets, taken once per Owner team.
func (s *service) covered(p authz.Principal, events []event.Event) []event.Event {
	allowed := make(map[string]bool)
	out := make([]event.Event, 0, len(events))
	for _, e := range events {
		ok, seen := allowed[e.OwnerTeam]
		if !seen {
			ok = s.authz.Allow(p, authz.Resource{Type: authz.TypeTicket, OwnerTeam: e.OwnerTeam}, authz.Read)
			allowed[e.OwnerTeam] = ok
		}
		if ok {
			out = append(out, e)
		}
	}
	return out
}

func (s *service) eventCounts(events []event.Event, now time.Time) EventCounts {
	out := EventCounts{Total: len(events), ByMonth: make([]MonthCount, EventMonths)}
	this := time.Date(now.In(s.loc).Year(), now.In(s.loc).Month(), 1, 0, 0, 0, 0, s.loc)
	index := make(map[string]int, EventMonths)
	for i := range EventMonths {
		month := this.AddDate(0, i-(EventMonths-1), 0).Format("2006-01")
		out.ByMonth[i].Month = month
		index[month] = i
	}
	for _, e := range events {
		start, end, dated := span(e)
		if !dated {
			continue
		}
		switch {
		case start.After(now):
			out.Upcoming++
		case !now.After(end):
			out.Live++
		}
		if i, ok := index[start.In(s.loc).Format("2006-01")]; ok {
			out.ByMonth[i].Count++
		}
	}
	return out
}

// span is when the Event runs: from its start to its end, or for half a
// day when it has no end. dated is false without a start.
func span(e event.Event) (start, end time.Time, dated bool) {
	if e.StartDate == nil {
		return time.Time{}, time.Time{}, false
	}
	start = *e.StartDate
	end = start.Add(undatedEventSpan)
	if e.EndDate != nil {
		end = *e.EndDate
	}
	return start, end, true
}

func startOfDay(t time.Time, loc *time.Location) time.Time {
	local := t.In(loc)
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
}

// dayKeys are the n days up to and including today, oldest first.
func dayKeys(today time.Time, n int) []string {
	out := make([]string, n)
	for i := range n {
		out[i] = today.AddDate(0, 0, i-(n-1)).Format(time.DateOnly)
	}
	return out
}
