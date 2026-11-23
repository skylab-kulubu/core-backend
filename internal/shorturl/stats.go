package shorturl

import (
	"context"
	"net/url"
	"sort"
	"strings"
	"time"
	_ "time/tzdata"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
)

const (
	// LinkStatsDays is how many days a link's summary covers when the caller
	// names none, today included.
	LinkStatsDays = 30
	// LinkStatsMaxDays is the longest summary: the click list's window.
	LinkStatsMaxDays = int(HitListWindow / (24 * time.Hour))
	// LinkStatsTimeZone is the zone the summary's days are counted in: the
	// club's.
	LinkStatsTimeZone = "Europe/Istanbul"
	// linkStatsTop is how many referring sites the summary names; the rest
	// are counted under OtherChannel.
	linkStatsTop = 10
)

// Devices a click's browser is counted under.
const (
	DeviceMobile  = "mobile"
	DeviceTablet  = "tablet"
	DeviceDesktop = "desktop"
	DeviceBot     = "bot"
	DeviceUnknown = "unknown"
)

// DayCount is the clicks of one calendar day (YYYY-MM-DD, in
// LinkStatsTimeZone).
type DayCount struct {
	Day   string `json:"day"`
	Count int    `json:"count"`
}

// NamedCount is the clicks under one name: a referring site or a device.
type NamedCount struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// LinkStats is one link's clicks over a window, counted and nothing else: no
// address, browser string, account or campaign of any click leaves core.
type LinkStats struct {
	Since time.Time `json:"since"`
	Days  int       `json:"days"`
	Total int       `json:"total"`
	// Scans is the part of Total that came from a QR code.
	Scans int `json:"scans"`
	// Daily has one entry for every day of the window, oldest first, zero
	// included.
	Daily []DayCount `json:"daily"`
	// Sources counts the clicks by channel (KeptSource: a known channel,
	// "other", or "" for an untagged click).
	Sources []SourceCount `json:"sources"`
	// Referrers counts the clicks by the referring site's host, without
	// "www."; "" is a click that named no site. Beyond the first ten, sites
	// are counted together under "other".
	Referrers []NamedCount `json:"referrers"`
	// Devices counts the clicks by the kind of device (DeviceMobile …).
	Devices []NamedCount `json:"devices"`
}

// LinkStats summarises one link's clicks over the last days calendar days,
// today included. A URL moderator reads any link's; the person who made a
// link reads their own. days is 1 to LinkStatsMaxDays; 0 means LinkStatsDays.
func (s *service) LinkStats(ctx context.Context, p authz.Principal, id uuid.UUID, days int) (LinkStats, error) {
	if days == 0 {
		days = LinkStatsDays
	}
	if days < 1 || days > LinkStatsMaxDays {
		return LinkStats{}, ErrInvalid
	}
	u, err := s.store.Get(ctx, id)
	if err != nil {
		return LinkStats{}, err
	}
	if !s.mayReadStats(p, u) {
		return LinkStats{}, ErrForbidden
	}
	loc, err := time.LoadLocation(LinkStatsTimeZone)
	if err != nil {
		return LinkStats{}, err
	}
	now := time.Now().In(loc)
	first := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, -(days - 1))
	hits, err := s.store.ListHits(ctx, id, first.UTC())
	if err != nil {
		return LinkStats{}, err
	}
	return summarise(hits, first, days, loc), nil
}

func (s *service) mayReadStats(p authz.Principal, u URL) bool {
	if s.authz.Allow(p, authz.Resource{Type: authz.TypeURL}, authz.Read) {
		return true
	}
	return u.CreatedBy != nil && p.ID != "" && u.CreatedBy.String() == p.ID &&
		s.authz.Allow(p, authz.Resource{Type: authz.TypeURL}, authz.ReadMe)
}

func summarise(hits []Hit, first time.Time, days int, loc *time.Location) LinkStats {
	out := LinkStats{Since: first.UTC(), Days: days, Daily: make([]DayCount, days)}
	index := make(map[string]int, days)
	for i := range out.Daily {
		day := first.AddDate(0, 0, i).Format(time.DateOnly)
		out.Daily[i] = DayCount{Day: day}
		index[day] = i
	}
	sources := map[string]*SourceCount{}
	referrers := map[string]int{}
	devices := map[string]int{}
	for _, h := range hits {
		i, ok := index[h.CreatedAt.In(loc).Format(time.DateOnly)]
		if !ok {
			continue
		}
		out.Daily[i].Count++
		out.Total++
		scan := h.scanned()
		if scan {
			out.Scans++
		}
		source := KeptSource(h.UTM.Source)
		c := sources[source]
		if c == nil {
			c = &SourceCount{Source: source}
			sources[source] = c
		}
		c.Count++
		if scan {
			c.Scans++
		}
		referrers[refererHost(h.Referer)]++
		devices[DeviceOf(h.UserAgent)]++
	}
	out.Sources = make([]SourceCount, 0, len(sources))
	for _, c := range sources {
		out.Sources = append(out.Sources, *c)
	}
	sort.Slice(out.Sources, func(i, j int) bool {
		a, b := out.Sources[i], out.Sources[j]
		if a.Count != b.Count {
			return a.Count > b.Count
		}
		return a.Source < b.Source
	})
	out.Referrers = topNamed(referrers, linkStatsTop)
	out.Devices = topNamed(devices, 0)
	return out
}

// topNamed orders counts by size, then name; with limit > 0 the names past
// it are counted together under OtherChannel.
func topNamed(counts map[string]int, limit int) []NamedCount {
	out := make([]NamedCount, 0, len(counts))
	for name, n := range counts {
		out = append(out, NamedCount{Name: name, Count: n})
	}
	sortNamed(out)
	if limit <= 0 || len(out) <= limit {
		return out
	}
	rest := 0
	for _, c := range out[limit:] {
		rest += c.Count
	}
	kept := out[:limit]
	for i := range kept {
		if kept[i].Name == OtherChannel {
			kept[i].Count += rest
			sortNamed(kept)
			return kept
		}
	}
	kept = append(kept, NamedCount{Name: OtherChannel, Count: rest})
	sortNamed(kept)
	return kept
}

func sortNamed(rows []NamedCount) {
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Count != rows[j].Count {
			return rows[i].Count > rows[j].Count
		}
		return rows[i].Name < rows[j].Name
	})
}

// refererHost is the referring site's host in lower case without "www.", or
// "" when the click named none (or nothing readable).
func refererHost(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	return strings.TrimPrefix(host, "www.")
}

// DeviceOf names the kind of device a browser string comes from. Link
// previews and crawlers (a WhatsApp or Telegram preview, a search engine)
// count as DeviceBot, so a chart can set them apart from people.
func DeviceOf(userAgent string) string {
	ua := strings.ToLower(strings.TrimSpace(userAgent))
	switch {
	case ua == "":
		return DeviceUnknown
	case containsAny(ua, "bot", "crawl", "spider", "preview", "facebookexternalhit", "whatsapp/", "curl/", "python", "go-http-client", "headless"):
		return DeviceBot
	case containsAny(ua, "ipad", "tablet") || (strings.Contains(ua, "android") && !strings.Contains(ua, "mobile")):
		return DeviceTablet
	case containsAny(ua, "mobi", "iphone", "ipod", "android"):
		return DeviceMobile
	case containsAny(ua, "windows", "macintosh", "mac os x", "x11", "linux", "cros"):
		return DeviceDesktop
	default:
		return DeviceUnknown
	}
}

func containsAny(s string, parts ...string) bool {
	for _, p := range parts {
		if strings.Contains(s, p) {
			return true
		}
	}
	return false
}
