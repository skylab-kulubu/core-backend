package competitor

import (
	"context"
	"errors"
	"sort"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
)

type Service interface {
	List(ctx context.Context) ([]Competitor, error)
	Get(ctx context.Context, id uuid.UUID) (Competitor, error)
	Create(ctx context.Context, p authz.Principal, in CreateInput) (Competitor, error)
	Update(ctx context.Context, p authz.Principal, id uuid.UUID, in UpdateInput) (Competitor, error)
	Delete(ctx context.Context, p authz.Principal, id uuid.UUID) error
	Mine(ctx context.Context, p authz.Principal) ([]Competitor, error)
	ListByEvent(ctx context.Context, eventID uuid.UUID) ([]Competitor, error)
	ListByUser(ctx context.Context, userID uuid.UUID) ([]Competitor, error)
	ListByOwnerTeam(ctx context.Context, ownerTeam string) ([]Competitor, error)
	Winner(ctx context.Context, eventID uuid.UUID) (Competitor, error)
	Leaderboard(ctx context.Context, eventType string, seasonID *uuid.UUID) ([]LeaderboardEntry, error)
}

type service struct {
	competitors Store
	events      event.Store
	authz       authz.Authorizer
}

func NewService(competitors Store, events event.Store, az authz.Authorizer) Service {
	return &service{competitors: competitors, events: events, authz: az}
}

func (s *service) withEvent(ctx context.Context, c Competitor) Competitor {
	ev, err := s.events.Get(ctx, c.EventID)
	if err != nil {
		return c
	}
	res := ev.Resource()
	c.Event = &res
	return c
}

func (s *service) withEvents(ctx context.Context, comps []Competitor) []Competitor {
	out := make([]Competitor, len(comps))
	for i, c := range comps {
		out[i] = s.withEvent(ctx, c)
	}
	return out
}

func (s *service) List(ctx context.Context) ([]Competitor, error) {
	if !s.authz.Allow(authz.Principal{}, authz.Resource{Type: authz.TypeCompetitor}, authz.Read) {
		return nil, ErrForbidden
	}
	comps, err := s.competitors.List(ctx)
	if err != nil {
		return nil, err
	}
	return s.withEvents(ctx, comps), nil
}

func (s *service) Get(ctx context.Context, id uuid.UUID) (Competitor, error) {
	if !s.authz.Allow(authz.Principal{}, authz.Resource{Type: authz.TypeCompetitor}, authz.Read) {
		return Competitor{}, ErrForbidden
	}
	got, err := s.competitors.Get(ctx, id)
	if err != nil {
		return Competitor{}, err
	}
	return s.withEvent(ctx, got), nil
}

func (s *service) Create(ctx context.Context, p authz.Principal, in CreateInput) (Competitor, error) {
	if in.UserID == uuid.Nil || in.EventID == uuid.Nil {
		return Competitor{}, ErrInvalid
	}
	ev, err := s.events.Get(ctx, in.EventID)
	if err != nil {
		if errors.Is(err, event.ErrNotFound) {
			return Competitor{}, ErrNotFound
		}
		return Competitor{}, err
	}
	if !ev.Active {
		return Competitor{}, ErrInvalid
	}
	res := authz.Resource{Type: authz.TypeCompetitor, OwnerTeam: ev.OwnerTeam, EventType: ev.OwnerTeam, OwnerID: in.UserID.String()}
	if !s.authz.Allow(p, res, authz.Create) {
		return Competitor{}, ErrForbidden
	}
	exists, err := s.competitors.ExistsUserEvent(ctx, in.UserID, in.EventID)
	if err != nil {
		return Competitor{}, err
	}
	if exists {
		return Competitor{}, ErrConflict
	}
	score := in.Score
	if p.ID == in.UserID.String() {
		score = nil
	}
	created, err := s.competitors.Create(ctx, Competitor{
		UserID:   in.UserID,
		EventID:  in.EventID,
		Score:    score,
		IsWinner: in.IsWinner,
	})
	if err != nil {
		return Competitor{}, err
	}
	return s.withEvent(ctx, created), nil
}

func (s *service) Update(ctx context.Context, p authz.Principal, id uuid.UUID, in UpdateInput) (Competitor, error) {
	existing, err := s.competitors.Get(ctx, id)
	if err != nil {
		return Competitor{}, err
	}
	if in.UserID == uuid.Nil {
		in.UserID = existing.UserID
	}
	if in.EventID == uuid.Nil {
		in.EventID = existing.EventID
	}
	ev, err := s.events.Get(ctx, in.EventID)
	if err != nil {
		if errors.Is(err, event.ErrNotFound) {
			return Competitor{}, ErrNotFound
		}
		return Competitor{}, err
	}
	res := authz.Resource{Type: authz.TypeCompetitor, OwnerTeam: ev.OwnerTeam, EventType: ev.OwnerTeam, OwnerID: existing.UserID.String()}
	if !s.authz.Allow(p, res, authz.Update) {
		return Competitor{}, ErrForbidden
	}
	if in.UserID != existing.UserID || in.EventID != existing.EventID {
		exists, err := s.competitors.ExistsUserEvent(ctx, in.UserID, in.EventID)
		if err != nil {
			return Competitor{}, err
		}
		if exists {
			return Competitor{}, ErrConflict
		}
	}
	existing.UserID = in.UserID
	existing.EventID = in.EventID
	existing.Score = in.Score
	existing.IsWinner = in.IsWinner
	updated, err := s.competitors.Update(ctx, existing)
	if err != nil {
		return Competitor{}, err
	}
	return s.withEvent(ctx, updated), nil
}

func (s *service) Delete(ctx context.Context, p authz.Principal, id uuid.UUID) error {
	existing, err := s.competitors.Get(ctx, id)
	if err != nil {
		return err
	}
	ev, err := s.events.Get(ctx, existing.EventID)
	if err != nil {
		if errors.Is(err, event.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	res := authz.Resource{Type: authz.TypeCompetitor, OwnerTeam: ev.OwnerTeam, EventType: ev.OwnerTeam, OwnerID: existing.UserID.String()}
	if !s.authz.Allow(p, res, authz.Delete) {
		return ErrForbidden
	}
	return s.competitors.Delete(ctx, id)
}

func (s *service) Mine(ctx context.Context, p authz.Principal) ([]Competitor, error) {
	if !s.authz.Allow(p, authz.Resource{Type: authz.TypeCompetitor}, authz.ReadMe) {
		return nil, ErrForbidden
	}
	userID, err := uuid.Parse(p.ID)
	if err != nil {
		return nil, ErrInvalid
	}
	comps, err := s.competitors.ListByUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	return s.withEvents(ctx, comps), nil
}

func (s *service) ListByEvent(ctx context.Context, eventID uuid.UUID) ([]Competitor, error) {
	if !s.authz.Allow(authz.Principal{}, authz.Resource{Type: authz.TypeCompetitor}, authz.Read) {
		return nil, ErrForbidden
	}
	if _, err := s.events.Get(ctx, eventID); err != nil {
		if errors.Is(err, event.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	comps, err := s.competitors.ListByEvent(ctx, eventID)
	if err != nil {
		return nil, err
	}
	return s.withEvents(ctx, comps), nil
}

func (s *service) ListByUser(ctx context.Context, userID uuid.UUID) ([]Competitor, error) {
	if !s.authz.Allow(authz.Principal{}, authz.Resource{Type: authz.TypeCompetitor}, authz.Read) {
		return nil, ErrForbidden
	}
	comps, err := s.competitors.ListByUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	return s.withEvents(ctx, comps), nil
}

func (s *service) ListByOwnerTeam(ctx context.Context, ownerTeam string) ([]Competitor, error) {
	if !s.authz.Allow(authz.Principal{}, authz.Resource{Type: authz.TypeCompetitor}, authz.Read) {
		return nil, ErrForbidden
	}
	if ownerTeam == "" {
		return nil, ErrInvalid
	}
	comps, err := s.competitors.ListByOwnerTeam(ctx, ownerTeam)
	if err != nil {
		return nil, err
	}
	return s.withEvents(ctx, comps), nil
}

func (s *service) Winner(ctx context.Context, eventID uuid.UUID) (Competitor, error) {
	if !s.authz.Allow(authz.Principal{}, authz.Resource{Type: authz.TypeCompetitor}, authz.Read) {
		return Competitor{}, ErrForbidden
	}
	if _, err := s.events.Get(ctx, eventID); err != nil {
		if errors.Is(err, event.ErrNotFound) {
			return Competitor{}, ErrNotFound
		}
		return Competitor{}, err
	}
	got, err := s.competitors.Winner(ctx, eventID)
	if err != nil {
		return Competitor{}, err
	}
	return s.withEvent(ctx, got), nil
}

func (s *service) Leaderboard(ctx context.Context, eventType string, seasonID *uuid.UUID) ([]LeaderboardEntry, error) {
	if !s.authz.Allow(authz.Principal{}, authz.Resource{Type: authz.TypeCompetitor}, authz.Read) {
		return nil, ErrForbidden
	}
	if eventType == "" {
		return nil, ErrInvalid
	}
	comps, err := s.competitors.ListByOwnerTeam(ctx, eventType)
	if err != nil {
		return nil, err
	}

	type agg struct {
		total float64
		count int
	}
	byUser := map[uuid.UUID]agg{}
	for _, c := range comps {
		ev, err := s.events.Get(ctx, c.EventID)
		if err != nil {
			continue
		}
		if seasonID != nil && (ev.SeasonID == nil || *ev.SeasonID != *seasonID) {
			continue
		}
		a := byUser[c.UserID]
		a.count++
		if c.Score != nil {
			a.total += *c.Score
		}
		byUser[c.UserID] = a
	}

	out := make([]LeaderboardEntry, 0, len(byUser))
	for userID, a := range byUser {
		out = append(out, LeaderboardEntry{UserID: userID, TotalScore: a.total, EventCount: a.count})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].TotalScore == out[j].TotalScore {
			return out[i].UserID.String() < out[j].UserID.String()
		}
		return out[i].TotalScore > out[j].TotalScore
	})
	assignRanks(out)
	return out, nil
}

func assignRanks(entries []LeaderboardEntry) {
	if len(entries) == 0 {
		return
	}
	entries[0].Rank = 1
	for i := 1; i < len(entries); i++ {
		if entries[i].TotalScore == entries[i-1].TotalScore {
			entries[i].Rank = entries[i-1].Rank
			continue
		}
		entries[i].Rank = i + 1
	}
}
