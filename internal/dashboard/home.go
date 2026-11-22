package dashboard

import (
	"context"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
)

const (
	// WeekDays is how many days the home page's week covers, today
	// included.
	WeekDays = 7
	// CompareDays is the length of the two periods Compare sets side by
	// side: the last CompareDays days and the CompareDays before them.
	CompareDays = 30
	// SoonDays is how far ahead an Event's gaps are a thing to do: today
	// and tomorrow.
	SoonDays = 2
	// WeekSessionLimit bounds the Sessions of the week.
	WeekSessionLimit = 60
)

// Missing names what an Event that starts soon still lacks.
const (
	MissingLocation = "location"
	MissingForm     = "form"
	MissingSessions = "sessions"
)

// Home is the admin panel's home page (docs/dashboard-summary.md, Home):
// what the caller has to do, what runs this week, and a few figures set
// against the period before.
type Home struct {
	// Todo is nil for a caller who can act on none of its items: who may
	// edit no Event, issue no certificate and handle no data subject
	// request.
	Todo *Todo `json:"todo"`
	Week Week  `json:"week"`
	// Compare is nil for a caller the summary covers no Event of and who
	// may not read people.
	Compare *Compare `json:"compare"`
	// Daily, Monthly, Teams, Sessions and Certificates are the covered
	// Events' charts (home_charts.go); empty or nil when none is covered.
	Daily        []DailyActivity    `json:"daily"`
	Monthly      []MonthlyActivity  `json:"monthly"`
	Teams        []TeamActivity     `json:"teams"`
	Sessions     int                `json:"sessions"`
	Certificates *CertificateCounts `json:"certificates"`
	// Season is the season running now, or else the one that ended last,
	// with its published Events; nil without one.
	Season *SeasonSummary `json:"season"`
	// Club counts the published Events, whoever owns them: what everyone
	// may see.
	Club Club `json:"club"`
}

// Todo are the things waiting on the caller, each with what it needs.
type Todo struct {
	// Drafts are the covered drafts the caller may edit (publish), by
	// start date, undated last.
	Drafts []TodoEvent `json:"drafts"`
	// Incomplete are the covered published Events the caller may edit that
	// run now or start today or tomorrow and lack a place, a form or a
	// Session.
	Incomplete []IncompleteEvent `json:"incomplete"`
	// FailedCertificates are the covered Events with certificate jobs that
	// failed, for a caller who may issue that Event's certificates.
	FailedCertificates []FailedCertificates `json:"failedCertificates"`
	// SubjectRequests is nil for a caller who may not handle data subject
	// requests.
	SubjectRequests *SubjectRequestCounts `json:"subjectRequests"`
}

type TodoEvent struct {
	ID        uuid.UUID  `json:"id"`
	Name      string     `json:"name"`
	OwnerTeam string     `json:"ownerTeam"`
	StartDate *time.Time `json:"startDate,omitempty"`
}

type IncompleteEvent struct {
	TodoEvent
	Live bool `json:"live"`
	// Missing are MissingLocation, MissingForm and MissingSessions, in
	// that order.
	Missing []string `json:"missing"`
}

type FailedCertificates struct {
	EventID   uuid.UUID `json:"eventId"`
	EventName string    `json:"eventName"`
	Failed    int       `json:"failed"`
}

// SubjectRequestCounts count the open (received or awaiting verification)
// data subject requests and those of them past their due day.
type SubjectRequestCounts struct {
	Open    int `json:"open"`
	Overdue int `json:"overdue"`
}

// Week is what runs from today to WeekDays days on: the published Events
// the caller may see and their Sessions.
type Week struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
	// Events run at some time in [From, To), by start.
	Events []WeekEvent `json:"events"`
	// Sessions start in [From, To), by start; at most WeekSessionLimit.
	Sessions []WeekSession `json:"sessions"`
}

type WeekEvent struct {
	ID        uuid.UUID  `json:"id"`
	Name      string     `json:"name"`
	OwnerTeam string     `json:"ownerTeam"`
	Location  string     `json:"location"`
	StartDate *time.Time `json:"startDate,omitempty"`
	EndDate   *time.Time `json:"endDate,omitempty"`
	Live      bool       `json:"live"`
}

// SessionSlot is one current Session of an Event's current day.
type SessionSlot struct {
	ID        uuid.UUID
	EventID   uuid.UUID
	Title     string
	StartTime time.Time
	EndTime   *time.Time
}

type WeekSession struct {
	ID        uuid.UUID  `json:"id"`
	EventID   uuid.UUID  `json:"eventId"`
	EventName string     `json:"eventName"`
	Title     string     `json:"title"`
	StartTime time.Time  `json:"startTime"`
	EndTime   *time.Time `json:"endTime,omitempty"`
}

// Compare sets the last CompareDays days against the CompareDays before.
type Compare struct {
	Days int `json:"days"`
	// Applications are the covered Events' Tickets that came in in the
	// period. The periods are calendar days in TimeZone: the last
	// CompareDays of Home.Daily, today included, and the CompareDays before.
	Applications Period `json:"applications"`
	// Attendees are the covered Events' Tickets checked in during the
	// period, each once however often it came: the applicant list's
	// "checked in" (TicketCounts.CheckedIn), for the period.
	Attendees Period `json:"attendees"`
	// Events are the covered published Events that started in the period.
	Events Period `json:"events"`
	// NewMembers are the active Members registered in the period; nil
	// when the caller may not read people or the tree could not be read.
	NewMembers *Period `json:"newMembers"`
}

type Period struct {
	Current  int `json:"current"`
	Previous int `json:"previous"`
}

// HomeStore reads what Home is made of beyond Store. Every method takes the
// Events the caller may see and answers for those alone.
type HomeStore interface {
	// SessionCounts counts each Event's current Sessions (not archived, not
	// cancelled, of a current day). An Event without one may be left out.
	SessionCounts(ctx context.Context, eventIDs []uuid.UUID) (map[uuid.UUID]int, error)
	// SessionsBetween are the Events' current Sessions that start in
	// [from, to), by start, at most limit.
	SessionsBetween(ctx context.Context, eventIDs []uuid.UUID, from, to time.Time, limit int) ([]SessionSlot, error)
	// FailedCertificateJobs counts each Event's failed certificate jobs.
	// An Event without one may be left out.
	FailedCertificateJobs(ctx context.Context, eventIDs []uuid.UUID) (map[uuid.UUID]int, error)
	// OpenSubjectRequests counts the open data subject requests and those
	// of them due before today ("2006-01-02").
	OpenSubjectRequests(ctx context.Context, today string) (SubjectRequestCounts, error)
	// DailyCheckIns counts, for each Event, the Tickets checked in on each
	// calendar day in loc ("2006-01-02") at or after since: a Ticket checked
	// in to two Sessions of a day counts once that day.
	DailyCheckIns(ctx context.Context, eventIDs []uuid.UUID, since time.Time, loc *time.Location) (map[uuid.UUID]map[string]int, error)
	// Attendees counts the Tickets checked in, each once however often it
	// came: per Event and calendar month in loc ("2006-01") and per Event
	// in all, at or after since; and over all the Events in [previous,
	// current) and at or after current.
	Attendees(ctx context.Context, eventIDs []uuid.UUID, since, previous, current time.Time, loc *time.Location) (AttendeeCounts, error)
	// CertificateCounts counts the Events' certificates: issued (revoked
	// ones too), revoked, and issued in [from, mid) and [mid, now].
	CertificateCounts(ctx context.Context, eventIDs []uuid.UUID, from, mid time.Time) (CertificateCounts, error)
}

// home builds Home. all are every current Event, covered the ones whose
// applications p may read; daily their applications per day since
// seriesStart.
func (s *service) home(ctx context.Context, p authz.Principal, now time.Time, all, covered []event.Event, daily map[uuid.UUID]map[string]int, members *Members, registered []time.Time) (*Home, error) {
	today := startOfDay(now, s.loc)
	out := &Home{
		Week:  Week{From: today.UTC(), To: today.AddDate(0, 0, WeekDays).UTC(), Events: []WeekEvent{}, Sessions: []WeekSession{}},
		Daily: []DailyActivity{}, Monthly: []MonthlyActivity{}, Teams: []TeamActivity{},
	}

	todo, err := s.todo(ctx, p, now, today, covered)
	if err != nil {
		return nil, err
	}
	out.Todo = todo

	if err := s.week(ctx, p, now, all, &out.Week); err != nil {
		return nil, err
	}
	if out.Season, err = s.season(ctx, p, now, all); err != nil {
		return nil, err
	}
	out.Club = s.club(p, now, all)

	if len(covered) > 0 || members != nil {
		cmp := &Compare{Days: CompareDays}
		mid := now.Add(-CompareDays * 24 * time.Hour)
		from := mid.Add(-CompareDays * 24 * time.Hour)
		if len(covered) > 0 {
			if err := s.activity(ctx, p, now, today, covered, daily, out, cmp); err != nil {
				return nil, err
			}
		}
		if members != nil {
			var p Period
			for _, at := range registered {
				switch {
				case !at.Before(mid) && !at.After(now):
					p.Current++
				case !at.Before(from) && at.Before(mid):
					p.Previous++
				}
			}
			cmp.NewMembers = &p
		}
		out.Compare = cmp
	}
	return out, nil
}

func (s *service) todo(ctx context.Context, p authz.Principal, now, today time.Time, covered []event.Event) (*Todo, error) {
	editable := map[string]bool{}
	issuable := map[string]bool{}
	canEdit := func(team string) bool {
		v, seen := editable[team]
		if !seen {
			v = s.authz.Allow(p, authz.Resource{Type: authz.TypeEvent, OwnerTeam: team}, authz.Update)
			editable[team] = v
		}
		return v
	}
	canIssue := func(team string) bool {
		v, seen := issuable[team]
		if !seen {
			v = s.authz.Allow(p, authz.Resource{Type: authz.TypeCertificate, OwnerTeam: team}, authz.Issue)
			issuable[team] = v
		}
		return v
	}
	handlesRequests := p.Product == "" && s.authz.Allow(p, authz.Resource{Type: authz.TypeSubjectRequest}, authz.Update)

	acts := handlesRequests
	out := &Todo{Drafts: []TodoEvent{}, Incomplete: []IncompleteEvent{}, FailedCertificates: []FailedCertificates{}}
	soonEnd := today.AddDate(0, 0, SoonDays)
	soon := make([]event.Event, 0)
	issued := make([]uuid.UUID, 0)
	names := map[uuid.UUID]string{}
	for _, e := range covered {
		edit := canEdit(e.OwnerTeam)
		issue := canIssue(e.OwnerTeam)
		acts = acts || edit || issue
		if issue {
			issued = append(issued, e.ID)
			names[e.ID] = e.Name
		}
		if !edit {
			continue
		}
		switch e.Status {
		case event.StatusDraft:
			out.Drafts = append(out.Drafts, TodoEvent{ID: e.ID, Name: e.Name, OwnerTeam: e.OwnerTeam, StartDate: e.StartDate})
		case event.StatusPublished:
			start, end, dated := span(e)
			if dated && start.Before(soonEnd) && !now.After(end) {
				soon = append(soon, e)
			}
		}
	}
	if !acts {
		return nil, nil
	}
	sort.SliceStable(out.Drafts, func(i, j int) bool {
		a, b := out.Drafts[i].StartDate, out.Drafts[j].StartDate
		if a == nil || b == nil {
			return a != nil
		}
		return a.Before(*b)
	})

	if len(soon) > 0 {
		ids := make([]uuid.UUID, len(soon))
		for i, e := range soon {
			ids[i] = e.ID
		}
		sessions, err := s.homeStore.SessionCounts(ctx, ids)
		if err != nil {
			return nil, err
		}
		sort.SliceStable(soon, func(i, j int) bool { return soon[i].StartDate.Before(*soon[j].StartDate) })
		for _, e := range soon {
			missing := make([]string, 0, 3)
			if e.Location == "" {
				missing = append(missing, MissingLocation)
			}
			if e.FormURL == "" && len(e.ExtraFormURLs) == 0 {
				missing = append(missing, MissingForm)
			}
			if sessions[e.ID] == 0 {
				missing = append(missing, MissingSessions)
			}
			if len(missing) == 0 {
				continue
			}
			start, _, _ := span(e)
			out.Incomplete = append(out.Incomplete, IncompleteEvent{
				TodoEvent: TodoEvent{ID: e.ID, Name: e.Name, OwnerTeam: e.OwnerTeam, StartDate: e.StartDate},
				Live:      !start.After(now),
				Missing:   missing,
			})
		}
	}

	if len(issued) > 0 {
		failed, err := s.homeStore.FailedCertificateJobs(ctx, issued)
		if err != nil {
			return nil, err
		}
		for _, id := range issued {
			if n := failed[id]; n > 0 {
				out.FailedCertificates = append(out.FailedCertificates, FailedCertificates{EventID: id, EventName: names[id], Failed: n})
			}
		}
	}

	if handlesRequests {
		counts, err := s.homeStore.OpenSubjectRequests(ctx, today.Format(time.DateOnly))
		if err != nil {
			return nil, err
		}
		out.SubjectRequests = &counts
	}
	return out, nil
}

// week fills w with the published Events p may see that run in it, and
// their Sessions that start in it. A published Event is public, whoever
// owns it, so the week is the club's, not only the covered teams'.
func (s *service) week(ctx context.Context, p authz.Principal, now time.Time, all []event.Event, w *Week) error {
	ids := make([]uuid.UUID, 0)
	names := map[uuid.UUID]string{}
	for _, e := range all {
		if e.Status != event.StatusPublished || !event.VisibleTo(s.authz, &p, e) {
			continue
		}
		start, end, dated := span(e)
		if !dated || !start.Before(w.To) || end.Before(w.From) {
			continue
		}
		w.Events = append(w.Events, WeekEvent{
			ID: e.ID, Name: e.Name, OwnerTeam: e.OwnerTeam, Location: e.Location,
			StartDate: e.StartDate, EndDate: e.EndDate,
			Live: !start.After(now) && !now.After(end),
		})
		ids = append(ids, e.ID)
		names[e.ID] = e.Name
	}
	sort.SliceStable(w.Events, func(i, j int) bool { return w.Events[i].StartDate.Before(*w.Events[j].StartDate) })
	if len(ids) == 0 {
		return nil
	}
	slots, err := s.homeStore.SessionsBetween(ctx, ids, w.From, w.To, WeekSessionLimit)
	if err != nil {
		return err
	}
	for _, slot := range slots {
		w.Sessions = append(w.Sessions, WeekSession{
			ID: slot.ID, EventID: slot.EventID, EventName: names[slot.EventID], Title: slot.Title,
			StartTime: slot.StartTime.UTC(), EndTime: slot.EndTime,
		})
	}
	return nil
}
