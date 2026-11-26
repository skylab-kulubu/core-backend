package raffle

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Candidate is an active Ticket of the Event as the criteria read it.
type Candidate struct {
	TicketID   uuid.UUID
	TicketType string
	OwnerID    *uuid.UUID
	// Name is the holder's: a guest's on the Ticket, a member's account
	// name. Email is a guest's address on the Ticket, a member's account
	// address, lowercased.
	Name  string
	Email string
	// Sessions are the live Sessions it checked in on, each once; Entered
	// the dates (YYYY-MM-DD) of its Venue entries.
	Sessions []uuid.UUID
	Entered  []string
}

// Store keeps the raffles and reads their candidates.
type Store interface {
	// Candidates are the Event's active Tickets (cancelled and waitlisted
	// ones never take part).
	Candidates(ctx context.Context, eventID uuid.UUID) ([]Candidate, error)
	// PastWinners are the Tickets that hold a winner's place in one of the
	// Event's raffles.
	PastWinners(ctx context.Context, eventID uuid.UUID) (map[uuid.UUID]bool, error)
	// Draw runs build under the Event's raffle lock, with the past winners
	// read under it, and keeps the raffle it returns with its entries: two
	// draws at once never both pass over the same past winners.
	Draw(ctx context.Context, eventID uuid.UUID, build func(pastWinners map[uuid.UUID]bool) (Raffle, error)) (Raffle, error)
	List(ctx context.Context, eventID uuid.UUID) ([]Summary, error)
	// Get is the raffle with its entries and eligible list; ErrNotFound for
	// one of another Event.
	Get(ctx context.Context, eventID, raffleID uuid.UUID) (Raffle, error)
	// Forfeit marks the winner at position forfeited with reason and makes
	// the first alternate left a winner, both at once: ErrNotWinner when
	// position holds no winner, ErrNoAlternates when none is left (nothing
	// changes). It answers the promoted position.
	Forfeit(ctx context.Context, eventID, raffleID uuid.UUID, position int, reason string, by uuid.UUID, at time.Time) (int, error)
}

// Memory is a Store in memory, for tests.
type Memory struct {
	// draw is the Event's raffle lock (one draw at a time); mu guards the
	// data, never held while a draw builds.
	draw       sync.Mutex
	mu         sync.Mutex
	candidates map[uuid.UUID][]Candidate
	raffles    []Raffle
	// People names the drawers and forfeiters (by id), as users would.
	People map[uuid.UUID]string
}

// NewMemory is an empty Memory.
func NewMemory() *Memory {
	return &Memory{candidates: map[uuid.UUID][]Candidate{}, People: map[uuid.UUID]string{}}
}

// SetCandidates sets the Event's active Tickets.
func (m *Memory) SetCandidates(eventID uuid.UUID, c []Candidate) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.candidates[eventID] = slices.Clone(c)
}

func (m *Memory) Candidates(_ context.Context, eventID uuid.UUID) ([]Candidate, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.candidates[eventID]), nil
}

func (m *Memory) PastWinners(_ context.Context, eventID uuid.UUID) (map[uuid.UUID]bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pastWinners(eventID), nil
}

func (m *Memory) pastWinners(eventID uuid.UUID) map[uuid.UUID]bool {
	out := map[uuid.UUID]bool{}
	for _, r := range m.raffles {
		if r.EventID != eventID {
			continue
		}
		for _, e := range r.Entries {
			if e.Status == StatusWinner {
				out[e.TicketID] = true
			}
		}
	}
	return out
}

func (m *Memory) Draw(_ context.Context, eventID uuid.UUID, build func(map[uuid.UUID]bool) (Raffle, error)) (Raffle, error) {
	m.draw.Lock()
	defer m.draw.Unlock()
	m.mu.Lock()
	past := m.pastWinners(eventID)
	m.mu.Unlock()
	r, err := build(past)
	if err != nil {
		return Raffle{}, err
	}
	r.Entries = slices.Clone(r.Entries)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.raffles = append(m.raffles, r)
	return m.read(len(m.raffles) - 1), nil
}

// read is raffle i as Get answers it: names filled from the candidates.
func (m *Memory) read(i int) Raffle {
	r := m.raffles[i]
	r.Entries = slices.Clone(r.Entries)
	byID := map[uuid.UUID]Candidate{}
	for _, c := range m.candidates[r.EventID] {
		byID[c.TicketID] = c
	}
	for j := range r.Entries {
		e := &r.Entries[j]
		c := byID[e.TicketID]
		e.Name, e.email, e.TicketType = c.Name, c.Email, c.TicketType
		if e.ForfeitedBy != nil {
			e.ForfeitedBy = &Person{ID: e.ForfeitedBy.ID, Name: m.People[e.ForfeitedBy.ID]}
		}
	}
	if r.DrawnBy != nil {
		r.DrawnBy = &Person{ID: r.DrawnBy.ID, Name: m.People[r.DrawnBy.ID]}
	}
	return r
}

func (m *Memory) List(_ context.Context, eventID uuid.UUID) ([]Summary, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []Summary{}
	for i := len(m.raffles) - 1; i >= 0; i-- {
		if m.raffles[i].EventID != eventID {
			continue
		}
		out = append(out, summaryOf(m.read(i)))
	}
	return out, nil
}

func (m *Memory) Get(_ context.Context, eventID, raffleID uuid.UUID) (Raffle, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, r := range m.raffles {
		if r.ID == raffleID && r.EventID == eventID {
			return m.read(i), nil
		}
	}
	return Raffle{}, ErrNotFound
}

func (m *Memory) Forfeit(_ context.Context, eventID, raffleID uuid.UUID, position int, reason string, by uuid.UUID, at time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.raffles {
		r := &m.raffles[i]
		if r.ID != raffleID || r.EventID != eventID {
			continue
		}
		won, next := -1, -1
		for j, e := range r.Entries {
			if e.Position == position && e.Status == StatusWinner {
				won = j
			}
			if e.Status == StatusAlternate && (next < 0 || e.Position < r.Entries[next].Position) {
				next = j
			}
		}
		switch {
		case won < 0:
			return 0, ErrNotWinner
		case next < 0:
			return 0, ErrNoAlternates
		}
		when := at
		r.Entries[won].Status, r.Entries[won].ForfeitedAt = StatusForfeited, &when
		r.Entries[won].ForfeitReason, r.Entries[won].ForfeitedBy = &reason, &Person{ID: by}
		r.Entries[next].Status, r.Entries[next].PromotedAt = StatusWinner, &when
		return r.Entries[next].Position, nil
	}
	return 0, ErrNotFound
}

func summaryOf(r Raffle) Summary {
	return Summary{
		ID: r.ID, Title: r.Title, Criteria: r.Criteria, EligibleCount: r.EligibleCount,
		WinnerCount: r.WinnerCount, AlternateCount: r.AlternateCount, DrawnAt: r.DrawnAt, DrawnBy: r.DrawnBy,
	}
}

// lowerEmail is an address as the exclusions compare it.
func lowerEmail(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
