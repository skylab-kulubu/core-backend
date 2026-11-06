package competition

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/competition/scoring"
	"github.com/skylab-kulubu/core-backend/internal/lifecycle"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// UserReader reads a Ticket owner's name and account state.
type UserReader interface {
	Get(ctx context.Context, id uuid.UUID) (user.User, error)
}

// MemoryStore is the Store for tests and the in-memory server. It keeps the
// same rules as the database: one Entrant per Ticket, one live manual score
// per Entrant and Stage, rows only revoked.
type MemoryStore struct {
	mu           sync.Mutex
	tickets      ticket.Store
	users        UserReader
	competitions map[uuid.UUID]Competition
	entrants     map[uuid.UUID][]memoryEntrant
	entries      map[uuid.UUID][]Entry
}

type memoryEntrant struct {
	Entrant
	ticketIDs []uuid.UUID
}

func NewMemoryStore(tickets ticket.Store, users UserReader) *MemoryStore {
	return &MemoryStore{
		tickets: tickets, users: users,
		competitions: map[uuid.UUID]Competition{},
		entrants:     map[uuid.UUID][]memoryEntrant{},
		entries:      map[uuid.UUID][]Entry{},
	}
}

func cloneCompetition(c Competition) Competition {
	c.Stages = slices.Clone(c.Stages)
	return c
}

func (s *MemoryStore) Create(_ context.Context, c Competition, first Stage) (Competition, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	c.ID = uuid.New()
	c.CreatedAt, c.UpdatedAt = now, now
	first.ID = uuid.New()
	first.CompetitionID = c.ID
	first.CreatedAt, first.UpdatedAt = now, now
	c.Stages = []Stage{first}
	s.competitions[c.ID] = c
	return cloneCompetition(c), nil
}

func (s *MemoryStore) get(id uuid.UUID, includeArchived bool) (Competition, error) {
	c, ok := s.competitions[id]
	if !ok || (!includeArchived && c.ArchivedAt != nil) {
		return Competition{}, ErrNotFound
	}
	return cloneCompetition(c), nil
}

func (s *MemoryStore) Get(_ context.Context, id uuid.UUID) (Competition, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.get(id, false)
}

func (s *MemoryStore) GetIncludingArchived(_ context.Context, id uuid.UUID) (Competition, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.get(id, true)
}

func (s *MemoryStore) ListByEvent(_ context.Context, eventID uuid.UUID, visibility lifecycle.Visibility) ([]Competition, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Competition{}
	for _, c := range s.competitions {
		if c.EventID == eventID && visibility.Matches(c.ArchivedAt != nil) {
			out = append(out, cloneCompetition(c))
		}
	}
	slices.SortFunc(out, func(a, b Competition) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return out, nil
}

func (s *MemoryStore) Update(_ context.Context, c Competition) (Competition, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	got, err := s.get(c.ID, false)
	if err != nil {
		return Competition{}, err
	}
	got.Name, got.Description, got.UpdatedAt = c.Name, c.Description, time.Now().UTC()
	s.competitions[c.ID] = got
	return cloneCompetition(got), nil
}

func (s *MemoryStore) Transition(_ context.Context, id uuid.UUID, from []Status, to Status, reason string) (Competition, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	got, err := s.get(id, false)
	if err != nil {
		return Competition{}, err
	}
	if !slices.Contains(from, got.Status) {
		return Competition{}, ErrStatus
	}
	got.Status, got.UpdatedAt = to, time.Now().UTC()
	if to == StatusCancelled {
		got.CancelReason = reason
	}
	s.competitions[id] = got
	return cloneCompetition(got), nil
}

func (s *MemoryStore) Archive(_ context.Context, id uuid.UUID, actor *uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	got, err := s.get(id, true)
	if err != nil {
		return err
	}
	if got.ArchivedAt == nil {
		now := time.Now().UTC()
		got.ArchivedAt, got.ArchivedBy, got.UpdatedAt = &now, actor, now
		s.competitions[id] = got
	}
	return nil
}

func (s *MemoryStore) Restore(_ context.Context, id uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	got, err := s.get(id, true)
	if err != nil {
		return err
	}
	got.ArchivedAt, got.ArchivedBy = nil, nil
	s.competitions[id] = got
	return nil
}

func (s *MemoryStore) Tickets(ctx context.Context, ids []uuid.UUID) ([]TicketRef, error) {
	out := make([]TicketRef, 0, len(ids))
	for _, id := range ids {
		t, err := s.tickets.Get(ctx, id)
		if errors.Is(err, ticket.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, TicketRef{ID: t.ID, EventID: t.EventID})
	}
	return out, nil
}

func (s *MemoryStore) AddIndividuals(_ context.Context, competitionID uuid.UUID, ticketIDs []uuid.UUID, _ *uuid.UUID) ([]uuid.UUID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.competitions[competitionID]; !ok {
		return nil, ErrNotFound
	}
	for _, e := range s.entrants[competitionID] {
		for _, tid := range ticketIDs {
			if slices.Contains(e.ticketIDs, tid) {
				return nil, ErrTicketTaken
			}
		}
	}
	now := time.Now().UTC()
	out := make([]uuid.UUID, 0, len(ticketIDs))
	for i, tid := range ticketIDs {
		e := memoryEntrant{
			Entrant: Entrant{
				ID: uuid.New(), CompetitionID: competitionID, Kind: KindIndividual, Status: EntrantActive,
				// Keeps the Tickets' order among Entrants written at once.
				CreatedAt: now.Add(time.Duration(i) * time.Microsecond),
			},
			ticketIDs: []uuid.UUID{tid},
		}
		s.entrants[competitionID] = append(s.entrants[competitionID], e)
		out = append(out, e.ID)
	}
	return out, nil
}

func (s *MemoryStore) Entrants(ctx context.Context, competitionID uuid.UUID) ([]Entrant, error) {
	s.mu.Lock()
	list := slices.Clone(s.entrants[competitionID])
	s.mu.Unlock()
	out := make([]Entrant, 0, len(list))
	for _, e := range list {
		entrant := e.Entrant
		entrant.Members = make([]Member, 0, len(e.ticketIDs))
		for _, tid := range e.ticketIDs {
			m, err := s.member(ctx, tid)
			if err != nil {
				return nil, err
			}
			entrant.Members = append(entrant.Members, m)
		}
		entrant.DisplayName = entrantName(entrant)
		out = append(out, entrant)
	}
	return out, nil
}

func (s *MemoryStore) member(ctx context.Context, ticketID uuid.UUID) (Member, error) {
	t, err := s.tickets.Get(ctx, ticketID)
	if err != nil {
		return Member{}, err
	}
	var state user.AccountState
	var first, last string
	if t.OwnerID != nil {
		u, err := s.users.Get(ctx, *t.OwnerID)
		if err != nil && !errors.Is(err, user.ErrNotFound) {
			return Member{}, err
		}
		state, first, last = u.AccountState, u.FirstName, u.LastName
	}
	return Member{
		TicketID: t.ID, TicketType: t.TicketType,
		DisplayName: memberName(t.TicketType, state, first, last, t.GuestFirstName, t.GuestLastName),
		OwnerID:     t.OwnerID,
	}, nil
}

func (s *MemoryStore) Candidates(ctx context.Context, competitionID, eventID uuid.UUID) ([]Candidate, error) {
	s.mu.Lock()
	taken := map[uuid.UUID]bool{}
	for _, e := range s.entrants[competitionID] {
		for _, tid := range e.ticketIDs {
			taken[tid] = true
		}
	}
	s.mu.Unlock()
	tickets, err := s.tickets.ListByEvent(ctx, eventID)
	if err != nil {
		return nil, err
	}
	out := make([]Candidate, 0, len(tickets))
	for _, t := range tickets {
		m, err := s.member(ctx, t.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, Candidate{
			TicketID: t.ID, DisplayName: m.DisplayName, TicketType: t.TicketType,
			CheckedIn: len(t.CheckIns) > 0, Entrant: taken[t.ID],
		})
	}
	return out, nil
}

// entrantName is a team's name, or its one member's.
func entrantName(e Entrant) string {
	if e.Kind == KindTeam {
		return e.TeamName
	}
	if len(e.Members) == 0 {
		return user.DeletedDisplayName
	}
	return e.Members[0].DisplayName
}

func (s *MemoryStore) SetManualScore(_ context.Context, in ManualScore) (Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows := s.entries[in.CompetitionID]
	live := -1
	for i, row := range rows {
		if row.StageID == in.StageID && row.EntrantID == in.EntrantID && row.Source == scoring.SourceManual && row.RevokedAt == nil {
			live = i
		}
	}
	if err := checkLive(in, live >= 0, func() *Entry { e := rows[live]; return &e }); err != nil {
		return Entry{}, err
	}
	now := time.Now().UTC()
	var replaces *uuid.UUID
	if live >= 0 {
		rows[live].RevokedAt, rows[live].RevokedBy, rows[live].RevokeReason = &now, in.Actor, in.Reason
		rows[live].RevokedBasis = in.Basis
		id := rows[live].ID
		replaces = &id
	}
	points := in.Points
	entry := Entry{
		ID: uuid.New(), CompetitionID: in.CompetitionID, StageID: in.StageID, EntrantID: in.EntrantID,
		Source: scoring.SourceManual, Points: &points, OccurredAt: now, RecordedAt: now,
		RecordedBy: in.Actor, Basis: in.Basis, ReplacesID: replaces,
	}
	s.entries[in.CompetitionID] = append(rows, entry)
	return entry, nil
}

func (s *MemoryStore) Revoke(_ context.Context, competitionID, entryID uuid.UUID, actor *uuid.UUID, basis, reason string) (Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows := s.entries[competitionID]
	for i, row := range rows {
		if row.ID != entryID {
			continue
		}
		if row.RevokedAt != nil {
			return Entry{}, ErrConflict
		}
		now := time.Now().UTC()
		rows[i].RevokedAt, rows[i].RevokedBy, rows[i].RevokeReason = &now, actor, reason
		rows[i].RevokedBasis = basis
		return rows[i], nil
	}
	return Entry{}, ErrNotFound
}

func (s *MemoryStore) Entries(_ context.Context, competitionID uuid.UUID) ([]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := slices.Clone(s.entries[competitionID])
	if out == nil {
		out = []Entry{}
	}
	return out, nil
}
