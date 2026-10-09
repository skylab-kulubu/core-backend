package teammail

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Action is what happened to the membership. Its value is what the template
// reads: `added` renders "eklendi", anything else "çıkarıldı".
type Action string

const (
	ActionAdded   Action = "added"
	ActionRemoved Action = "removed"
)

// Change is one queued team membership mail. It holds ids and the Group's
// path, never an address or a name: those are read when the mail is sent,
// so a person erased meanwhile gets nothing.
type Change struct {
	ID        uuid.UUID
	SubjectID uuid.UUID
	// ActorID is who made the change, nil when the caller's subject was not
	// a UUID. A service account's subject is one too; it has no name to
	// show, so its mail leaves LeaderName empty all the same.
	ActorID    *uuid.UUID
	GroupPath  string
	Action     Action
	OccurredAt time.Time
	// NextAttemptAt is when the change is due; Attempts counts its failed
	// sends in a row.
	NextAttemptAt time.Time
	Attempts      int
	// ClaimedAt is when the claim that returned the change was made. Complete
	// and Retry write only while the row still carries it: once another
	// core has claimed the row again, a late write of this one changes
	// nothing.
	ClaimedAt time.Time
}

// Queue keeps the changes waiting for their mail (PostgresQueue keeps them in
// team_membership_mails).
type Queue interface {
	// Enqueue queues change, due at once. A change already waiting for the
	// same person, Group and action is not queued again.
	Enqueue(ctx context.Context, change Change) error
	// Claim takes up to limit due changes, oldest due first, and holds them
	// for lease: another pass (another core) does not take them meanwhile.
	Claim(ctx context.Context, now time.Time, lease time.Duration, limit int) ([]Change, error)
	// Complete removes a claimed change whose mail is done with.
	Complete(ctx context.Context, claimed Change) error
	// Retry counts a failure on a claimed change and makes it due at at.
	Retry(ctx context.Context, claimed Change, at time.Time) error
	// DropBefore removes the changes that happened before occurredBefore.
	DropBefore(ctx context.Context, occurredBefore time.Time) (int, error)
	// Backlog counts the changes and the age of the oldest.
	Backlog(ctx context.Context, now time.Time) (int, time.Duration, error)
}

// MemoryQueue is a Queue in memory, for tests and a core without a database.
type MemoryQueue struct {
	mu      sync.Mutex
	changes []Change
}

func NewMemoryQueue() *MemoryQueue { return &MemoryQueue{} }

// Snapshot is a copy of the queued changes, in queue order.
func (q *MemoryQueue) Snapshot() []Change {
	q.mu.Lock()
	defer q.mu.Unlock()
	return slices.Clone(q.changes)
}

func (q *MemoryQueue) Enqueue(_ context.Context, change Change) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, waiting := range q.changes {
		if waiting.SubjectID == change.SubjectID && waiting.GroupPath == change.GroupPath && waiting.Action == change.Action {
			return nil
		}
	}
	change.NextAttemptAt = change.OccurredAt
	change.ClaimedAt = time.Time{}
	q.changes = append(q.changes, change)
	return nil
}

func (q *MemoryQueue) Claim(_ context.Context, now time.Time, lease time.Duration, limit int) ([]Change, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	due := make([]int, 0)
	for i, c := range q.changes {
		if !c.NextAttemptAt.After(now) {
			due = append(due, i)
		}
	}
	slices.SortStableFunc(due, func(a, b int) int { return q.changes[a].NextAttemptAt.Compare(q.changes[b].NextAttemptAt) })
	out := make([]Change, 0, min(limit, len(due)))
	for _, i := range due[:min(limit, len(due))] {
		q.changes[i].NextAttemptAt = now.Add(lease)
		q.changes[i].ClaimedAt = now
		out = append(out, q.changes[i])
	}
	return out, nil
}

func (q *MemoryQueue) Complete(_ context.Context, claimed Change) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.changes = slices.DeleteFunc(q.changes, func(c Change) bool { return c.ID == claimed.ID && c.ClaimedAt.Equal(claimed.ClaimedAt) })
	return nil
}

func (q *MemoryQueue) Retry(_ context.Context, claimed Change, at time.Time) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i := range q.changes {
		if q.changes[i].ID == claimed.ID && q.changes[i].ClaimedAt.Equal(claimed.ClaimedAt) {
			q.changes[i].Attempts++
			q.changes[i].NextAttemptAt = at
		}
	}
	return nil
}

func (q *MemoryQueue) DropBefore(_ context.Context, occurredBefore time.Time) (int, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	before := len(q.changes)
	q.changes = slices.DeleteFunc(q.changes, func(c Change) bool { return c.OccurredAt.Before(occurredBefore) })
	return before - len(q.changes), nil
}

func (q *MemoryQueue) Backlog(_ context.Context, now time.Time) (int, time.Duration, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.changes) == 0 {
		return 0, 0, nil
	}
	oldest := q.changes[0].OccurredAt
	for _, c := range q.changes {
		if c.OccurredAt.Before(oldest) {
			oldest = c.OccurredAt
		}
	}
	return len(q.changes), max(now.Sub(oldest), 0), nil
}

var _ Queue = (*MemoryQueue)(nil)
