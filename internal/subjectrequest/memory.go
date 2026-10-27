package subjectrequest

import (
	"context"
	"slices"
	"sync"

	"github.com/google/uuid"
)

// MemoryStore keeps the register in memory, for tests.
type MemoryStore struct {
	mu     sync.Mutex
	byID   map[uuid.UUID]Request
	events map[uuid.UUID][]Event
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{byID: map[uuid.UUID]Request{}, events: map[uuid.UUID][]Event{}}
}

func (s *MemoryStore) List(_ context.Context, limit int) ([]Request, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Request, 0, len(s.byID))
	for _, r := range s.byID {
		out = append(out, r)
	}
	slices.SortFunc(out, compareForList)
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// compareForList is the list's order: open requests first, soonest due
// first; then the concluded, latest answered first.
func compareForList(a, b Request) int {
	switch {
	case a.AnsweredOn == nil && b.AnsweredOn != nil:
		return -1
	case a.AnsweredOn != nil && b.AnsweredOn == nil:
		return 1
	case a.AnsweredOn == nil:
		if c := compareStrings(a.DueOn, b.DueOn); c != 0 {
			return c
		}
	default:
		if c := compareStrings(*b.AnsweredOn, *a.AnsweredOn); c != 0 {
			return c
		}
	}
	return compareStrings(b.ReceivedOn, a.ReceivedOn)
}

func compareStrings(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func (s *MemoryStore) Get(_ context.Context, id uuid.UUID) (Request, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.byID[id]
	if !ok {
		return Request{}, ErrNotFound
	}
	return r, nil
}

func (s *MemoryStore) Events(_ context.Context, id uuid.UUID) ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.events[id]), nil
}

func (s *MemoryStore) Create(_ context.Context, r Request, e Event) (Request, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r.DueOn = DueOn(r.ReceivedOn)
	s.byID[r.ID] = r
	s.events[r.ID] = append(s.events[r.ID], e)
	return r, nil
}

func (s *MemoryStore) Update(_ context.Context, r Request, e Event) (Request, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.byID[r.ID]; !ok {
		return Request{}, ErrNotFound
	}
	r.DueOn = DueOn(r.ReceivedOn)
	s.byID[r.ID] = r
	s.events[r.ID] = append(s.events[r.ID], e)
	return r, nil
}
