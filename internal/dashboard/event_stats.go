package dashboard

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
)

// EventStatsAnswer is the answer of GET /v1/dashboard/event-stats.
type EventStatsAnswer struct {
	GeneratedAt time.Time `json:"generatedAt"`
	// TimeZone is the zone the days of DailyApplications are counted in.
	TimeZone string `json:"timeZone"`
	// EventStats are every covered Event, past, still to come and undated
	// alike, by start date, undated ones last by name.
	EventStats []EventStat `json:"eventStats"`
}

// EventStats is Summary's eventStats without its date limit: the panel's
// Event list shows every Event, the long past and the undated too. It
// covers the Events Summary does (covered) and reads only their Ticket
// counts and last EventDailyDays days of applications, one query each.
func (s *service) EventStats(ctx context.Context, p authz.Principal) (EventStatsAnswer, error) {
	now := s.now()
	out := EventStatsAnswer{GeneratedAt: now.UTC(), TimeZone: TimeZone, EventStats: make([]EventStat, 0)}
	all, err := s.events.ListIncludingDrafts(ctx, "")
	if err != nil {
		return EventStatsAnswer{}, err
	}
	covered := s.covered(p, all)
	if len(covered) == 0 {
		return out, nil
	}
	ids := make([]uuid.UUID, len(covered))
	for i, e := range covered {
		ids[i] = e.ID
	}
	counts, err := s.store.TicketCounts(ctx, ids)
	if err != nil {
		return EventStatsAnswer{}, err
	}
	today := startOfDay(now, s.loc)
	days := dayKeys(today, EventDailyDays)
	daily, err := s.store.DailyApplications(ctx, ids, today.AddDate(0, 0, -(EventDailyDays-1)), s.loc)
	if err != nil {
		return EventStatsAnswer{}, err
	}
	out.EventStats = make([]EventStat, 0, len(covered))
	for _, e := range covered {
		out.EventStats = append(out.EventStats, eventStat(e, now, counts[e.ID], daily[e.ID], days))
	}
	sortEventStats(out.EventStats)
	return out, nil
}
