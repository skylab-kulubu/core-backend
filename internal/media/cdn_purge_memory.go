package media

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"
)

// MemoryCDNPurgeQueue is the CDN purge queue in memory, as PostgresStore
// keeps it in media_cdn_purges: one entry per address, leased while a pass
// purges it. It is for tests; core keeps its queue in the database.
type MemoryCDNPurgeQueue struct {
	mu         sync.Mutex
	entries    map[string]*memoryCDNPurge
	failQueued error
}

type memoryCDNPurge struct {
	entry CDNPurgeEntry
	next  time.Time
}

func NewMemoryCDNPurgeQueue() *MemoryCDNPurgeQueue {
	return &MemoryCDNPurgeQueue{entries: map[string]*memoryCDNPurge{}}
}

// FailEnqueue makes every later EnqueueCDNPurges fail with err (nil heals
// it).
func (q *MemoryCDNPurgeQueue) FailEnqueue(err error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.failQueued = err
}

func (q *MemoryCDNPurgeQueue) EnqueueCDNPurges(_ context.Context, urls []string, now time.Time) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.failQueued != nil {
		return q.failQueued
	}
	for _, url := range urls {
		q.entries[url] = &memoryCDNPurge{entry: CDNPurgeEntry{URL: url, QueuedAt: now}, next: now}
	}
	return nil
}

func (q *MemoryCDNPurgeQueue) ClaimCDNPurges(_ context.Context, now time.Time, lease time.Duration, limit int) ([]CDNPurgeEntry, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	var due []*memoryCDNPurge
	for _, e := range q.entries {
		if !e.next.After(now) {
			due = append(due, e)
		}
	}
	slices.SortFunc(due, func(a, b *memoryCDNPurge) int {
		if c := a.next.Compare(b.next); c != 0 {
			return c
		}
		return strings.Compare(a.entry.URL, b.entry.URL)
	})
	var out []CDNPurgeEntry
	for _, e := range due {
		if len(out) == limit {
			break
		}
		e.next = now.Add(lease)
		out = append(out, e.entry)
	}
	return out, nil
}

func (q *MemoryCDNPurgeQueue) CompleteCDNPurges(_ context.Context, done []CDNPurgeEntry) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, d := range done {
		if e, ok := q.entries[d.URL]; ok && e.entry.QueuedAt.Equal(d.QueuedAt) {
			delete(q.entries, d.URL)
		}
	}
	return nil
}

func (q *MemoryCDNPurgeQueue) RetryCDNPurges(_ context.Context, failed []CDNPurgeEntry, at []time.Time) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i, f := range failed {
		if e, ok := q.entries[f.URL]; ok && e.entry.QueuedAt.Equal(f.QueuedAt) {
			e.entry.Attempts++
			e.next = at[i]
		}
	}
	return nil
}

func (q *MemoryCDNPurgeQueue) DropCDNPurges(_ context.Context, queuedBefore time.Time) (int, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	dropped := 0
	for url, e := range q.entries {
		if e.entry.QueuedAt.Before(queuedBefore) {
			delete(q.entries, url)
			dropped++
		}
	}
	return dropped, nil
}

func (q *MemoryCDNPurgeQueue) CDNPurgeBacklog(_ context.Context, now time.Time) (int, time.Duration, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	var oldest time.Duration
	for _, e := range q.entries {
		oldest = max(oldest, now.Sub(e.entry.QueuedAt))
	}
	return len(q.entries), oldest, nil
}

// URLs are the queued addresses, sorted.
func (q *MemoryCDNPurgeQueue) URLs() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]string, 0, len(q.entries))
	for url := range q.entries {
		out = append(out, url)
	}
	slices.Sort(out)
	return out
}

// Attempts are the failed purges in a row of a queued address.
func (q *MemoryCDNPurgeQueue) Attempts(url string) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	if e, ok := q.entries[url]; ok {
		return e.entry.Attempts
	}
	return 0
}

var _ CDNPurgeQueue = (*MemoryCDNPurgeQueue)(nil)
