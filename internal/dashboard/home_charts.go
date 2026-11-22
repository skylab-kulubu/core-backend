package dashboard

import (
	"context"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/season"
)

const (
	// SeriesDays is how many days Home.Daily covers, today included: the
	// last CompareDays and the CompareDays before them.
	SeriesDays = 2 * CompareDays
	// SeriesMonths is how many months Home.Monthly, Home.Teams and
	// Home.Club cover, this one included.
	SeriesMonths = 12
	// SeasonEventLimit bounds the Events of Home.Season.
	SeasonEventLimit = 200
)

// DailyActivity is one day of the covered Events: the Tickets that came
// in, and the Tickets checked in that day (once a day each).
type DailyActivity struct {
	Date         string `json:"date"`
	Applications int    `json:"applications"`
	Attendees    int    `json:"attendees"`
}

// MonthlyActivity is one month of the covered Events: the Tickets that came
// in, the check-ins summed over its days (a Ticket counts once a day), and
// the published Events that started in it.
type MonthlyActivity struct {
	Month        string `json:"month"`
	Applications int    `json:"applications"`
	Attendees    int    `json:"attendees"`
	Events       int    `json:"events"`
}

// TeamActivity is one Owner team over the SeriesMonths months: its covered
// published Events that started in them, and their Tickets and check-ins
// in them. Team is "" for Events without an Owner team.
type TeamActivity struct {
	Team         string `json:"team"`
	Events       int    `json:"events"`
	Applications int    `json:"applications"`
	Attendees    int    `json:"attendees"`
}

// CertificateCounts are the certificates of the Events the caller may read
// the certificates of.
type CertificateCounts struct {
	Issued  int    `json:"issued"`
	Revoked int    `json:"revoked"`
	Period  Period `json:"period"`
}

// SeasonLister lists the seasons: season.Store.
type SeasonLister interface {
	List(ctx context.Context, activeOnly bool) ([]season.Season, error)
}

type SeasonSummary struct {
	ID        uuid.UUID     `json:"id"`
	Name      string        `json:"name"`
	StartDate time.Time     `json:"startDate"`
	EndDate   time.Time     `json:"endDate"`
	Events    []SeasonEvent `json:"events"`
}

type SeasonEvent struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	OwnerTeam string    `json:"ownerTeam"`
	StartDate time.Time `json:"startDate"`
}

// Club is what every signed-in person may see: the published Events by
// the month they start in and by Owner team, over SeriesMonths months.
type Club struct {
	EventsByMonth []MonthCount `json:"eventsByMonth"`
	EventsByTeam  []TeamCount  `json:"eventsByTeam"`
}

type TeamCount struct {
	Team  string `json:"team"`
	Count int    `json:"count"`
}

// seriesStart is the first day of the month SeriesMonths-1 months before
// today's: where the charts begin.
func seriesStart(today time.Time) time.Time {
	return time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, today.Location()).AddDate(0, -(SeriesMonths - 1), 0)
}

func monthKeys(today time.Time) []string {
	first := seriesStart(today)
	out := make([]string, SeriesMonths)
	for i := range SeriesMonths {
		out[i] = first.AddDate(0, i, 0).Format("2006-01")
	}
	return out
}

// activity fills the covered Events' charts and the applications and
// attendees of cmp.
func (s *service) activity(ctx context.Context, p authz.Principal, now, today time.Time, covered []event.Event, daily map[uuid.UUID]map[string]int, out *Home, cmp *Compare) error {
	start := seriesStart(today)
	ids := make([]uuid.UUID, len(covered))
	for i, e := range covered {
		ids[i] = e.ID
	}
	checkIns, err := s.homeStore.DailyCheckIns(ctx, ids, start, s.loc)
	if err != nil {
		return err
	}

	days := dayKeys(today, SeriesDays)
	out.Daily = make([]DailyActivity, len(days))
	for i, day := range days {
		out.Daily[i].Date = day
		for _, e := range covered {
			out.Daily[i].Applications += daily[e.ID][day]
			out.Daily[i].Attendees += checkIns[e.ID][day]
		}
		if i < CompareDays {
			cmp.Applications.Previous += out.Daily[i].Applications
			cmp.Attendees.Previous += out.Daily[i].Attendees
		} else {
			cmp.Applications.Current += out.Daily[i].Applications
			cmp.Attendees.Current += out.Daily[i].Attendees
		}
	}

	months := monthKeys(today)
	index := make(map[string]int, len(months))
	out.Monthly = make([]MonthlyActivity, len(months))
	for i, month := range months {
		out.Monthly[i].Month = month
		index[month] = i
	}
	teams := map[string]*TeamActivity{}
	team := func(name string) *TeamActivity {
		if teams[name] == nil {
			teams[name] = &TeamActivity{Team: name}
		}
		return teams[name]
	}
	mid := now.Add(-CompareDays * 24 * time.Hour)
	from := mid.Add(-CompareDays * 24 * time.Hour)
	for _, e := range covered {
		for day, n := range daily[e.ID] {
			if i, ok := index[day[:7]]; ok {
				out.Monthly[i].Applications += n
				team(e.OwnerTeam).Applications += n
			}
		}
		for day, n := range checkIns[e.ID] {
			if i, ok := index[day[:7]]; ok {
				out.Monthly[i].Attendees += n
				team(e.OwnerTeam).Attendees += n
			}
		}
		if e.Status != event.StatusPublished || e.StartDate == nil {
			continue
		}
		start := *e.StartDate
		if !start.Before(mid) && !start.After(now) {
			cmp.Events.Current++
		} else if !start.Before(from) && start.Before(mid) {
			cmp.Events.Previous++
		}
		if start.After(now) {
			continue
		}
		if i, ok := index[start.In(s.loc).Format("2006-01")]; ok {
			out.Monthly[i].Events++
			team(e.OwnerTeam).Events++
		}
	}
	out.Teams = make([]TeamActivity, 0, len(teams))
	for _, t := range teams {
		out.Teams = append(out.Teams, *t)
	}
	sort.Slice(out.Teams, func(i, j int) bool {
		a, b := out.Teams[i], out.Teams[j]
		if a.Applications != b.Applications {
			return a.Applications > b.Applications
		}
		return a.Team < b.Team
	})

	sessions, err := s.homeStore.SessionCounts(ctx, ids)
	if err != nil {
		return err
	}
	for _, n := range sessions {
		out.Sessions += n
	}

	readable := make([]uuid.UUID, 0, len(covered))
	allowed := map[string]bool{}
	for _, e := range covered {
		v, seen := allowed[e.OwnerTeam]
		if !seen {
			v = s.authz.Allow(p, authz.Resource{Type: authz.TypeCertificate, OwnerTeam: e.OwnerTeam}, authz.Read)
			allowed[e.OwnerTeam] = v
		}
		if v {
			readable = append(readable, e.ID)
		}
	}
	if len(readable) > 0 {
		counts, err := s.homeStore.CertificateCounts(ctx, readable, from, mid)
		if err != nil {
			return err
		}
		out.Certificates = &counts
	}
	return nil
}

// season is the season running now (today within its dates), or else the
// one that ended last, with the published Events of it p may see. Seasons
// without both dates are skipped.
func (s *service) season(ctx context.Context, p authz.Principal, now time.Time, all []event.Event) (*SeasonSummary, error) {
	if s.seasons == nil {
		return nil, nil
	}
	seasons, err := s.seasons.List(ctx, false)
	if err != nil {
		return nil, err
	}
	var running, ended *season.Season
	for i := range seasons {
		se := &seasons[i]
		if se.StartDate == nil || se.EndDate == nil || se.ArchivedAt != nil {
			continue
		}
		end := se.EndDate.Add(24 * time.Hour)
		switch {
		case !now.Before(*se.StartDate) && !now.After(end):
			if running == nil || se.StartDate.After(*running.StartDate) {
				running = se
			}
		case se.EndDate.Before(now):
			if ended == nil || se.EndDate.After(*ended.EndDate) {
				ended = se
			}
		}
	}
	picked := running
	if picked == nil {
		picked = ended
	}
	if picked == nil {
		return nil, nil
	}
	out := &SeasonSummary{ID: picked.ID, Name: picked.Name, StartDate: picked.StartDate.UTC(), EndDate: picked.EndDate.UTC(), Events: []SeasonEvent{}}
	for _, e := range all {
		if e.SeasonID == nil || *e.SeasonID != picked.ID || e.StartDate == nil ||
			e.Status != event.StatusPublished || !event.VisibleTo(s.authz, &p, e) {
			continue
		}
		out.Events = append(out.Events, SeasonEvent{ID: e.ID, Name: e.Name, OwnerTeam: e.OwnerTeam, StartDate: e.StartDate.UTC()})
	}
	sort.SliceStable(out.Events, func(i, j int) bool { return out.Events[i].StartDate.Before(out.Events[j].StartDate) })
	if len(out.Events) > SeasonEventLimit {
		out.Events = out.Events[:SeasonEventLimit]
	}
	return out, nil
}

// club counts the published Events that started, or start, in the
// SeriesMonths months up to this one's end.
func (s *service) club(p authz.Principal, now time.Time, all []event.Event) Club {
	today := startOfDay(now, s.loc)
	months := monthKeys(today)
	index := make(map[string]int, len(months))
	out := Club{EventsByMonth: make([]MonthCount, len(months)), EventsByTeam: []TeamCount{}}
	for i, month := range months {
		out.EventsByMonth[i].Month = month
		index[month] = i
	}
	teams := map[string]int{}
	for _, e := range all {
		if e.Status != event.StatusPublished || e.StartDate == nil || !event.VisibleTo(s.authz, &p, e) {
			continue
		}
		if i, ok := index[e.StartDate.In(s.loc).Format("2006-01")]; ok {
			out.EventsByMonth[i].Count++
			teams[e.OwnerTeam]++
		}
	}
	for team, n := range teams {
		out.EventsByTeam = append(out.EventsByTeam, TeamCount{Team: team, Count: n})
	}
	sort.Slice(out.EventsByTeam, func(i, j int) bool {
		a, b := out.EventsByTeam[i], out.EventsByTeam[j]
		if a.Count != b.Count {
			return a.Count > b.Count
		}
		return a.Team < b.Team
	})
	return out
}
