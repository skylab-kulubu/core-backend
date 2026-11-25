package competitor

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/lifecycle"
)

type Service interface {
	List(ctx context.Context, p authz.Principal) ([]Competitor, error)
	ListLifecycle(ctx context.Context, p authz.Principal, visibility lifecycle.Visibility) ([]Competitor, error)
	Get(ctx context.Context, p authz.Principal, id uuid.UUID) (Competitor, error)
	Create(ctx context.Context, p authz.Principal, in CreateInput) (Competitor, error)
	Update(ctx context.Context, p authz.Principal, id uuid.UUID, in UpdateInput) (Competitor, error)
	Delete(ctx context.Context, p authz.Principal, id uuid.UUID) error
	Reinstate(ctx context.Context, p authz.Principal, id uuid.UUID) (Competitor, error)
	Mine(ctx context.Context, p authz.Principal) ([]Competitor, error)
	ListByEvent(ctx context.Context, p authz.Principal, eventID uuid.UUID) ([]Competitor, error)
	ListByUser(ctx context.Context, p authz.Principal, userID uuid.UUID) ([]Competitor, error)
	ListByOwnerTeam(ctx context.Context, p authz.Principal, ownerTeam string) ([]Competitor, error)
	Winner(ctx context.Context, p authz.Principal, eventID uuid.UUID) (Competitor, error)
	Leaderboard(ctx context.Context, p authz.Principal, ownerTeam string, seasonID *uuid.UUID) ([]LeaderboardEntry, error)
}

type service struct {
	competitors Store
	events      event.Store
	authz       authz.Authorizer
}

func NewService(competitors Store, events event.Store, az authz.Authorizer) Service {
	return &service{competitors: competitors, events: events, authz: az}
}

// visibleEvent is the current Event id names, ErrNotFound when p may not see
// it: a draft does not exist for who may not see it (event.VisibleTo, ADR
// 0066), and neither do its Competitors.
func (s *service) visibleEvent(ctx context.Context, p authz.Principal, id uuid.UUID) (event.Event, error) {
	ev, err := s.events.Get(ctx, id)
	if err != nil {
		if errors.Is(err, event.ErrNotFound) {
			return event.Event{}, ErrNotFound
		}
		return event.Event{}, err
	}
	if !event.VisibleTo(s.authz, &p, ev) {
		return event.Event{}, ErrNotFound
	}
	return ev, nil
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

// withEvents adds each Competitor's Event, leaving out those whose Event p
// may not see (a draft) or that is gone.
func (s *service) withEvents(ctx context.Context, p authz.Principal, comps []Competitor) []Competitor {
	out := make([]Competitor, 0, len(comps))
	for _, c := range comps {
		ev, err := s.events.Get(ctx, c.EventID)
		if err != nil || !event.VisibleTo(s.authz, &p, ev) {
			continue
		}
		res := ev.Resource()
		c.Event = &res
		out = append(out, c)
	}
	return out
}

func (s *service) List(ctx context.Context, p authz.Principal) ([]Competitor, error) {
	if !s.authz.Allow(p, authz.Resource{Type: authz.TypeCompetitor}, authz.Read) {
		return nil, ErrForbidden
	}
	comps, err := s.competitors.List(ctx)
	if err != nil {
		return nil, err
	}
	return s.withEvents(ctx, p, comps), nil
}

func (s *service) ListLifecycle(ctx context.Context, p authz.Principal, visibility lifecycle.Visibility) ([]Competitor, error) {
	items, err := s.competitors.ListLifecycle(ctx, visibility)
	if err != nil {
		return nil, err
	}
	visible := make([]Competitor, 0, len(items))
	for _, item := range items {
		ev, err := s.events.GetIncludingArchived(ctx, item.EventID)
		if err != nil || !event.VisibleTo(s.authz, &p, ev) {
			continue
		}
		if s.authz.Allow(p, lifecycleResource(p, ev, item), authz.Delete) {
			eventResource := ev.Resource()
			item.Event = &eventResource
			visible = append(visible, item)
		}
	}
	return visible, nil
}

func (s *service) Get(ctx context.Context, p authz.Principal, id uuid.UUID) (Competitor, error) {
	got, err := s.competitors.Get(ctx, id)
	if err != nil {
		return Competitor{}, err
	}
	ev, err := s.visibleEvent(ctx, p, got.EventID)
	if err != nil {
		return Competitor{}, err
	}
	res := competitorResource(ev, got.UserID.String())
	if !s.authz.Allow(p, res, authz.Read) {
		return Competitor{}, ErrForbidden
	}
	return s.withEvent(ctx, got), nil
}

func (s *service) Create(ctx context.Context, p authz.Principal, in CreateInput) (Competitor, error) {
	if in.UserID == uuid.Nil || in.EventID == uuid.Nil {
		return Competitor{}, ErrInvalid
	}
	ev, err := s.visibleEvent(ctx, p, in.EventID)
	if err != nil {
		return Competitor{}, err
	}
	// A published Event, ended or not: results come after it (ADR 0066).
	if err := ev.StatusError(); err != nil {
		return Competitor{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	res := competitorResource(ev, in.UserID.String())
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
	// Who enters themselves gives themselves no score and no win: the
	// Event's organizers set both (Update).
	score, winner := in.Score, in.IsWinner
	if p.ID == in.UserID.String() {
		score, winner = nil, false
	}
	created, err := s.competitors.Create(ctx, Competitor{
		UserID:   in.UserID,
		EventID:  in.EventID,
		Score:    score,
		IsWinner: winner,
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
	// Moving it to another Event takes it out of the one it is in: who
	// moves it writes that Event's competitors too.
	if in.EventID != existing.EventID {
		from, err := s.events.GetIncludingArchived(ctx, existing.EventID)
		if err != nil {
			if errors.Is(err, event.ErrNotFound) {
				return Competitor{}, ErrNotFound
			}
			return Competitor{}, err
		}
		if !event.VisibleTo(s.authz, &p, from) {
			return Competitor{}, ErrNotFound
		}
		res := competitorResource(from, existing.UserID.String())
		if !s.authz.Allow(p, res, authz.Update) {
			return Competitor{}, ErrForbidden
		}
	}
	ev, err := s.visibleEvent(ctx, p, in.EventID)
	if err != nil {
		return Competitor{}, err
	}
	res := competitorResource(ev, existing.UserID.String())
	if !s.authz.Allow(p, res, authz.Update) {
		return Competitor{}, ErrForbidden
	}
	// Nobody writes the score or the win of their own competitor, nor takes
	// someone else's row with its result: a Leader and Privileged neither
	// (as the Competition ledger's own_entrant).
	if ownResult(p, existing, in) {
		return Competitor{}, ErrOwnCompetitor
	}
	// It moves only to a published Event, ended or not, as Create and
	// Reinstate ask (ADR 0066); an edit that keeps the Event is no move.
	if in.EventID != existing.EventID {
		if err := ev.StatusError(); err != nil {
			return Competitor{}, fmt.Errorf("%w: %w", ErrConflict, err)
		}
	}
	// A cancelled Event takes no one new, as Create refuses it: its
	// competitor keeps its person, its score and win stay editable. A
	// draft's competitor may still change person.
	if in.UserID != existing.UserID && ev.Status == event.StatusCancelled {
		return Competitor{}, fmt.Errorf("%w: %w", ErrConflict, event.ErrCancelled)
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
	existing, err := s.competitors.GetIncludingWithdrawn(ctx, id)
	if err != nil {
		return err
	}
	ev, err := s.events.GetIncludingArchived(ctx, existing.EventID)
	if err != nil {
		if errors.Is(err, event.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	if !event.VisibleTo(s.authz, &p, ev) {
		return ErrNotFound
	}
	res := competitorResource(ev, existing.UserID.String())
	if !s.authz.Allow(p, res, authz.Delete) {
		return ErrForbidden
	}
	return s.competitors.Withdraw(ctx, id, lifecycle.ActorID(p.ID))
}

func (s *service) Reinstate(ctx context.Context, p authz.Principal, id uuid.UUID) (Competitor, error) {
	existing, err := s.competitors.GetIncludingWithdrawn(ctx, id)
	if err != nil {
		return Competitor{}, err
	}
	ev, err := s.events.GetIncludingArchived(ctx, existing.EventID)
	if err != nil {
		if errors.Is(err, event.ErrNotFound) {
			return Competitor{}, ErrNotFound
		}
		return Competitor{}, err
	}
	if !event.VisibleTo(s.authz, &p, ev) {
		return Competitor{}, ErrNotFound
	}
	if !s.authz.Allow(p, lifecycleResource(p, ev, existing), authz.Delete) {
		return Competitor{}, ErrForbidden
	}
	if ev.ArchivedAt != nil {
		return Competitor{}, ErrConflict
	}
	if err := ev.StatusError(); err != nil {
		return Competitor{}, fmt.Errorf("%w: %w", ErrConflict, err)
	}
	if err := s.competitors.Reinstate(ctx, id); err != nil {
		return Competitor{}, err
	}
	got, err := s.competitors.Get(ctx, id)
	if err != nil {
		return Competitor{}, err
	}
	return s.withEvent(ctx, got), nil
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
	return s.withEvents(ctx, p, comps), nil
}

func (s *service) ListByEvent(ctx context.Context, p authz.Principal, eventID uuid.UUID) ([]Competitor, error) {
	ev, err := s.visibleEvent(ctx, p, eventID)
	if err != nil {
		return nil, err
	}
	comps, err := s.competitors.ListByEvent(ctx, eventID)
	if err != nil {
		return nil, err
	}
	res := competitorResource(ev, "")
	if s.authz.Allow(p, res, authz.Read) {
		return s.withEvents(ctx, p, comps), nil
	}
	own := make([]Competitor, 0)
	for _, c := range comps {
		if p.ID != "" && c.UserID.String() == p.ID {
			own = append(own, c)
		}
	}
	if len(own) == 0 {
		return nil, ErrForbidden
	}
	return s.withEvents(ctx, p, own), nil
}

func (s *service) ListByUser(ctx context.Context, p authz.Principal, userID uuid.UUID) ([]Competitor, error) {
	if p.ID != userID.String() && !s.authz.Allow(p, authz.Resource{Type: authz.TypeCompetitor}, authz.Read) {
		return nil, ErrForbidden
	}
	comps, err := s.competitors.ListByUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	return s.withEvents(ctx, p, comps), nil
}

// ListByOwnerTeam lists the competitors of ownerTeam's Events: all of them
// to who reads the team's (its members, Privileged); to anyone else only
// those of the team's Events p reads as one of their Co-organizers, decided
// per Event (docs/co-organizers.md), and ErrForbidden when there is none.
func (s *service) ListByOwnerTeam(ctx context.Context, p authz.Principal, ownerTeam string) ([]Competitor, error) {
	if ownerTeam == "" {
		return nil, ErrInvalid
	}
	teamWide := s.authz.Allow(p, authz.Resource{Type: authz.TypeCompetitor, OwnerTeam: ownerTeam}, authz.Read)
	comps, err := s.competitors.ListByOwnerTeam(ctx, ownerTeam)
	if err != nil {
		return nil, err
	}
	if teamWide {
		return s.withEvents(ctx, p, comps), nil
	}
	out := make([]Competitor, 0)
	for _, c := range comps {
		ev, err := s.events.Get(ctx, c.EventID)
		if err != nil || !event.VisibleTo(s.authz, &p, ev) || !s.authz.Allow(p, competitorResource(ev, ""), authz.Read) {
			continue
		}
		res := ev.Resource()
		c.Event = &res
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, ErrForbidden
	}
	return out, nil
}

func (s *service) Winner(ctx context.Context, p authz.Principal, eventID uuid.UUID) (Competitor, error) {
	ev, err := s.visibleEvent(ctx, p, eventID)
	if err != nil {
		return Competitor{}, err
	}
	if !s.authz.Allow(p, competitorResource(ev, ""), authz.Read) {
		return Competitor{}, ErrForbidden
	}
	got, err := s.competitors.Winner(ctx, eventID)
	if err != nil {
		return Competitor{}, err
	}
	return s.withEvent(ctx, got), nil
}

// Leaderboard is ownerTeam's standings over all its Events: the team's own
// (its members, Privileged). A Co-organizer of one of its Events reads that
// Event's competitors, not the team's standings (docs/co-organizers.md).
func (s *service) Leaderboard(ctx context.Context, p authz.Principal, ownerTeam string, seasonID *uuid.UUID) ([]LeaderboardEntry, error) {
	if !s.authz.Allow(p, authz.Resource{Type: authz.TypeCompetitor, OwnerTeam: ownerTeam}, authz.Read) {
		return nil, ErrForbidden
	}
	if ownerTeam == "" {
		return nil, ErrInvalid
	}
	comps, err := s.competitors.ListByOwnerTeam(ctx, ownerTeam)
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
		if err != nil || !event.VisibleTo(s.authz, &p, ev) {
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

// lifecycleResource is c as a withdrawal or its undoing decides it: a
// person withdraws their own competitor and brings it back only when they
// withdrew it themselves. One an organizer withdrew comes back through an
// organizer (no owner on the resource), so a person removed by the Event's
// managers does not put themselves, and the result they were given, back.
func lifecycleResource(p authz.Principal, ev event.Event, c Competitor) authz.Resource {
	res := competitorResource(ev, c.UserID.String())
	if c.WithdrawnAt != nil && (c.WithdrawnBy == nil || c.WithdrawnBy.String() != p.ID) {
		res.OwnerID = ""
	}
	return res
}

// ownResult reports whether an edit by p writes a result on p's own
// competitor: it changes the score or the win of a row that is p's, or
// makes a row p's while it carries a score or a win.
func ownResult(p authz.Principal, existing Competitor, in UpdateInput) bool {
	if p.ID == "" {
		return false
	}
	was, becomes := existing.UserID.String() == p.ID, in.UserID.String() == p.ID
	if !was && !becomes {
		return false
	}
	if !was && (in.Score != nil || in.IsWinner) {
		return true
	}
	return !sameScore(existing.Score, in.Score) || existing.IsWinner != in.IsWinner
}

func sameScore(a, b *float64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// competitorResource is the decision on a competitor of ev (ownerID its
// person, "" for none): ev's, its Co-organizers included
// (event.AuthzResource, docs/co-organizers.md).
func competitorResource(ev event.Event, ownerID string) authz.Resource {
	r := event.AuthzResource(ev, authz.TypeCompetitor, time.Now())
	r.OwnerID = ownerID
	return r
}
