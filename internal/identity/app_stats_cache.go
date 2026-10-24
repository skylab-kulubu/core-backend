package identity

import (
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// appStatsTTL is how long the app assignment list keeps a Group's counts
// (how many Groups are below it, how many enabled people it and they hold;
// ekip-yetkileri ticket 15, T4). Every row of a Group walks its subtree, two
// Keycloak reads a Group, so a list of SkyForms on every top Team would
// read most of the Group tree on each opening. A membership or role mapping
// change through core drops what is kept at once; one made in Keycloak's
// console shows within the minute, as the seat search's does.
const appStatsTTL = time.Minute

// appStatsEntries bounds the Groups kept: past it, all are dropped.
const appStatsEntries = seatGroupLimit

type cachedAppStats struct {
	at                  time.Time
	inheritsTo, members int
}

// appStatsCache is the Group counts the app assignment list and the add
// drawer's Group search keep, one read at a time per Group.
type appStatsCache struct {
	now    func() time.Time
	flight singleflight.Group

	mu         sync.Mutex
	generation uint64
	groups     map[string]cachedAppStats
}

func newAppStatsCache(now func() time.Time) *appStatsCache {
	if now == nil {
		now = time.Now
	}
	return &appStatsCache{now: now, groups: map[string]cachedAppStats{}}
}

// forget drops every count kept: a membership, a Group or a role mapping
// changed.
func (c *appStatsCache) forget() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.generation++
	clear(c.groups)
}

// of answers the counts of the Group groupID kept, or reads them with read;
// a failed read is not kept, nor one a forget overtook.
func (c *appStatsCache) of(groupID string, read func() (int, int, error)) (int, int, error) {
	c.mu.Lock()
	if e, ok := c.groups[groupID]; ok && !c.now().Before(e.at) && c.now().Sub(e.at) < appStatsTTL {
		c.mu.Unlock()
		return e.inheritsTo, e.members, nil
	}
	c.mu.Unlock()
	v, err, _ := c.flight.Do(groupID, func() (any, error) {
		c.mu.Lock()
		generation := c.generation
		c.mu.Unlock()
		at := c.now()
		inheritsTo, members, err := read()
		if err != nil {
			return nil, err
		}
		e := cachedAppStats{at: at, inheritsTo: inheritsTo, members: members}
		c.mu.Lock()
		if c.generation == generation {
			if len(c.groups) >= appStatsEntries {
				clear(c.groups)
			}
			c.groups[groupID] = e
		}
		c.mu.Unlock()
		return e, nil
	})
	if err != nil {
		return 0, 0, err
	}
	e := v.(cachedAppStats)
	return e.inheritsTo, e.members, nil
}
