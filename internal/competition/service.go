package competition

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/competition/scoring"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/lifecycle"
)

// Service is the organizers' side of competitions. Every call is decided by
// the authorizer on the Event's Owner team (authz.TypeCompetition): a
// Leader of that team, or competition:manage.
type Service interface {
	Create(ctx context.Context, p authz.Principal, eventID uuid.UUID, in CreateInput) (Competition, error)
	ListByEvent(ctx context.Context, p authz.Principal, eventID uuid.UUID, visibility lifecycle.Visibility) ([]Competition, error)
	Get(ctx context.Context, p authz.Principal, id uuid.UUID) (Competition, error)
	Update(ctx context.Context, p authz.Principal, id uuid.UUID, in UpdateInput) (Competition, error)

	Open(ctx context.Context, p authz.Principal, id uuid.UUID) (Competition, error)
	Start(ctx context.Context, p authz.Principal, id uuid.UUID) (Competition, error)
	Close(ctx context.Context, p authz.Principal, id uuid.UUID) (Competition, error)
	Reopen(ctx context.Context, p authz.Principal, id uuid.UUID) (Competition, error)
	Cancel(ctx context.Context, p authz.Principal, id uuid.UUID, reason string) (Competition, error)
	Archive(ctx context.Context, p authz.Principal, id uuid.UUID) error
	Restore(ctx context.Context, p authz.Principal, id uuid.UUID) (Competition, error)

	AddEntrants(ctx context.Context, p authz.Principal, id uuid.UUID, ticketIDs []uuid.UUID) ([]Entrant, error)
	Entrants(ctx context.Context, p authz.Principal, id uuid.UUID) ([]Entrant, error)

	SetScore(ctx context.Context, p authz.Principal, id uuid.UUID, in ScoreInput) (Entry, error)
	Revoke(ctx context.Context, p authz.Principal, id, entryID uuid.UUID, reason string) (Entry, error)
	Entries(ctx context.Context, p authz.Principal, id uuid.UUID) ([]Entry, error)
	Leaderboard(ctx context.Context, p authz.Principal, id uuid.UUID) (Leaderboard, error)
}

// Limits of what an organizer types in.
const (
	maxNameRunes        = 200
	maxDescriptionRunes = 5000
	maxReasonRunes      = 1000
	// MaxEntrantsPerRequest bounds one AddEntrants call.
	MaxEntrantsPerRequest = 500
)

// What each status allows (spec §7): Entrants join while the Competition is
// a draft, open or running; scores are written in every status but
// cancelled, closed included, since results are mostly typed in after the
// Event.
var (
	entrantStatuses = []Status{StatusDraft, StatusOpen, StatusRunning}
	scoreStatuses   = []Status{StatusDraft, StatusOpen, StatusRunning, StatusClosed}
)

type service struct {
	store  Store
	events event.Store
	authz  authz.Authorizer
}

func NewService(store Store, events event.Store, az authz.Authorizer) Service {
	return &service{store: store, events: events, authz: az}
}

// eventFor is the Competition's Event; an archived Event hides its
// Competitions (spec §7).
func (s *service) eventFor(ctx context.Context, eventID uuid.UUID) (event.Event, error) {
	if eventID == uuid.Nil {
		return event.Event{}, ErrNotFound
	}
	ev, err := s.events.Get(ctx, eventID)
	if errors.Is(err, event.ErrNotFound) {
		return event.Event{}, ErrNotFound
	}
	return ev, err
}

func (s *service) allow(p authz.Principal, ev event.Event, action authz.Action) error {
	if !s.authz.Allow(p, authz.Resource{Type: authz.TypeCompetition, OwnerTeam: ev.OwnerTeam}, action) {
		return ErrForbidden
	}
	return nil
}

// load is a Competition the caller may act on, with its Owner team.
func (s *service) load(ctx context.Context, p authz.Principal, id uuid.UUID, action authz.Action, includeArchived bool) (Competition, error) {
	get := s.store.Get
	if includeArchived {
		get = s.store.GetIncludingArchived
	}
	c, err := get(ctx, id)
	if err != nil {
		return Competition{}, err
	}
	ev, err := s.eventFor(ctx, c.EventID)
	if err != nil {
		return Competition{}, err
	}
	if err := s.allow(p, ev, action); err != nil {
		return Competition{}, err
	}
	c.OwnerTeam = ev.OwnerTeam
	return c, nil
}

func withOwner(c Competition, err error, owner string) (Competition, error) {
	if err != nil {
		return Competition{}, err
	}
	c.OwnerTeam = owner
	return c, nil
}

func cleanName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" || utf8.RuneCountInString(name) > maxNameRunes {
		return "", fmt.Errorf("%w: name is 1 to %d characters", ErrInvalid, maxNameRunes)
	}
	return name, nil
}

func cleanDescription(raw string) (string, error) {
	desc := strings.TrimSpace(raw)
	if utf8.RuneCountInString(desc) > maxDescriptionRunes {
		return "", fmt.Errorf("%w: description is at most %d characters", ErrInvalid, maxDescriptionRunes)
	}
	return desc, nil
}

// cleanReason is a reason the caller must give: ErrReasonRequired when
// empty.
func cleanReason(raw string) (string, error) {
	reason := strings.TrimSpace(raw)
	if reason == "" {
		return "", ErrReasonRequired
	}
	if utf8.RuneCountInString(reason) > maxReasonRunes {
		return "", fmt.Errorf("%w: reason is at most %d characters", ErrInvalid, maxReasonRunes)
	}
	return reason, nil
}

func (s *service) Create(ctx context.Context, p authz.Principal, eventID uuid.UUID, in CreateInput) (Competition, error) {
	ev, err := s.eventFor(ctx, eventID)
	if err != nil {
		return Competition{}, err
	}
	if err := s.allow(p, ev, authz.Create); err != nil {
		return Competition{}, err
	}
	name, err := cleanName(in.Name)
	if err != nil {
		return Competition{}, err
	}
	desc, err := cleanDescription(in.Description)
	if err != nil {
		return Competition{}, err
	}
	switch in.EntrantMode {
	case "", ModeIndividual:
	case ModeTeam, ModeBoth:
		return Competition{}, fmt.Errorf("%w: team entrants are not available yet", ErrInvalid)
	default:
		return Competition{}, fmt.Errorf("%w: unknown entrant mode", ErrInvalid)
	}
	strategy, _ := scoring.Lookup(scoring.SourceManual)
	config := json.RawMessage(`{}`)
	if err := strategy.Validate(config); err != nil {
		return Competition{}, err
	}
	created, err := s.store.Create(ctx, Competition{
		EventID: ev.ID, Name: name, Description: desc, EntrantMode: ModeIndividual,
		Status: StatusDraft, CreatedBy: lifecycle.ActorID(p.ID),
	}, Stage{Position: 1, Strategy: strategy.Key(), Config: config, ConfigVersion: 1})
	return withOwner(created, err, ev.OwnerTeam)
}

func (s *service) ListByEvent(ctx context.Context, p authz.Principal, eventID uuid.UUID, visibility lifecycle.Visibility) ([]Competition, error) {
	ev, err := s.eventFor(ctx, eventID)
	if err != nil {
		return nil, err
	}
	if err := s.allow(p, ev, authz.Read); err != nil {
		return nil, err
	}
	list, err := s.store.ListByEvent(ctx, ev.ID, visibility)
	if err != nil {
		return nil, err
	}
	for i := range list {
		list[i].OwnerTeam = ev.OwnerTeam
	}
	return list, nil
}

func (s *service) Get(ctx context.Context, p authz.Principal, id uuid.UUID) (Competition, error) {
	return s.load(ctx, p, id, authz.Read, false)
}

func (s *service) Update(ctx context.Context, p authz.Principal, id uuid.UUID, in UpdateInput) (Competition, error) {
	c, err := s.load(ctx, p, id, authz.Update, false)
	if err != nil {
		return Competition{}, err
	}
	if c.Status == StatusCancelled {
		return Competition{}, ErrStatus
	}
	if in.Name != nil {
		if c.Name, err = cleanName(*in.Name); err != nil {
			return Competition{}, err
		}
	}
	if in.Description != nil {
		if c.Description, err = cleanDescription(*in.Description); err != nil {
			return Competition{}, err
		}
	}
	updated, err := s.store.Update(ctx, c)
	return withOwner(updated, err, c.OwnerTeam)
}

func (s *service) transition(ctx context.Context, p authz.Principal, id uuid.UUID, from []Status, to Status, reason string) (Competition, error) {
	c, err := s.load(ctx, p, id, authz.Update, false)
	if err != nil {
		return Competition{}, err
	}
	moved, err := s.store.Transition(ctx, id, from, to, reason)
	return withOwner(moved, err, c.OwnerTeam)
}

func (s *service) Open(ctx context.Context, p authz.Principal, id uuid.UUID) (Competition, error) {
	return s.transition(ctx, p, id, []Status{StatusDraft}, StatusOpen, "")
}

func (s *service) Start(ctx context.Context, p authz.Principal, id uuid.UUID) (Competition, error) {
	return s.transition(ctx, p, id, []Status{StatusDraft, StatusOpen}, StatusRunning, "")
}

func (s *service) Close(ctx context.Context, p authz.Principal, id uuid.UUID) (Competition, error) {
	return s.transition(ctx, p, id, []Status{StatusDraft, StatusOpen, StatusRunning}, StatusClosed, "")
}

// Reopen takes a closed Competition back to running, to add a forgotten
// Entrant.
func (s *service) Reopen(ctx context.Context, p authz.Principal, id uuid.UUID) (Competition, error) {
	return s.transition(ctx, p, id, []Status{StatusClosed}, StatusRunning, "")
}

// Cancel is final and needs its reason.
func (s *service) Cancel(ctx context.Context, p authz.Principal, id uuid.UUID, reason string) (Competition, error) {
	if _, err := s.load(ctx, p, id, authz.Update, false); err != nil {
		return Competition{}, err
	}
	why, err := cleanReason(reason)
	if err != nil {
		return Competition{}, err
	}
	return s.transition(ctx, p, id, []Status{StatusDraft, StatusOpen, StatusRunning, StatusClosed}, StatusCancelled, why)
}

func (s *service) Archive(ctx context.Context, p authz.Principal, id uuid.UUID) error {
	if _, err := s.load(ctx, p, id, authz.Delete, true); err != nil {
		return err
	}
	return s.store.Archive(ctx, id, lifecycle.ActorID(p.ID))
}

func (s *service) Restore(ctx context.Context, p authz.Principal, id uuid.UUID) (Competition, error) {
	c, err := s.load(ctx, p, id, authz.Delete, true)
	if err != nil {
		return Competition{}, err
	}
	if err := s.store.Restore(ctx, id); err != nil {
		return Competition{}, err
	}
	restored, err := s.store.Get(ctx, id)
	return withOwner(restored, err, c.OwnerTeam)
}

func allowed(status Status, in []Status) bool {
	for _, s := range in {
		if s == status {
			return true
		}
	}
	return false
}

func (s *service) AddEntrants(ctx context.Context, p authz.Principal, id uuid.UUID, ticketIDs []uuid.UUID) ([]Entrant, error) {
	c, err := s.load(ctx, p, id, authz.Update, false)
	if err != nil {
		return nil, err
	}
	if !allowed(c.Status, entrantStatuses) {
		return nil, ErrStatus
	}
	if len(ticketIDs) == 0 || len(ticketIDs) > MaxEntrantsPerRequest {
		return nil, fmt.Errorf("%w: 1 to %d tickets", ErrInvalid, MaxEntrantsPerRequest)
	}
	seen := make(map[uuid.UUID]bool, len(ticketIDs))
	for _, tid := range ticketIDs {
		if tid == uuid.Nil || seen[tid] {
			return nil, fmt.Errorf("%w: a ticket is missing or repeated", ErrInvalid)
		}
		seen[tid] = true
	}
	refs, err := s.store.Tickets(ctx, ticketIDs)
	if err != nil {
		return nil, err
	}
	ofEvent := 0
	for _, ref := range refs {
		if ref.EventID == c.EventID {
			ofEvent++
		}
	}
	if ofEvent != len(ticketIDs) {
		return nil, fmt.Errorf("%w: every ticket must be a ticket of the competition's event", ErrInvalid)
	}
	newIDs, err := s.store.AddIndividuals(ctx, c.ID, ticketIDs, lifecycle.ActorID(p.ID))
	if err != nil {
		return nil, err
	}
	all, err := s.store.Entrants(ctx, c.ID)
	if err != nil {
		return nil, err
	}
	byID := make(map[uuid.UUID]Entrant, len(all))
	for _, e := range all {
		byID[e.ID] = e
	}
	out := make([]Entrant, 0, len(newIDs))
	for _, eid := range newIDs {
		out = append(out, byID[eid])
	}
	return out, nil
}

func (s *service) Entrants(ctx context.Context, p authz.Principal, id uuid.UUID) ([]Entrant, error) {
	c, err := s.load(ctx, p, id, authz.Read, false)
	if err != nil {
		return nil, err
	}
	return s.store.Entrants(ctx, c.ID)
}

// stageOf is the Stage named, or the first one.
func stageOf(c Competition, stageID *uuid.UUID) (Stage, error) {
	if len(c.Stages) == 0 {
		return Stage{}, fmt.Errorf("competition %s has no stage", c.ID)
	}
	if stageID == nil {
		return c.Stages[0], nil
	}
	for _, st := range c.Stages {
		if st.ID == *stageID {
			return st, nil
		}
	}
	return Stage{}, fmt.Errorf("%w: not a stage of this competition", ErrInvalid)
}

func (s *service) SetScore(ctx context.Context, p authz.Principal, id uuid.UUID, in ScoreInput) (Entry, error) {
	c, err := s.load(ctx, p, id, authz.Update, false)
	if err != nil {
		return Entry{}, err
	}
	if !allowed(c.Status, scoreStatuses) {
		return Entry{}, ErrStatus
	}
	stage, err := stageOf(c, in.StageID)
	if err != nil {
		return Entry{}, err
	}
	if stage.Strategy != scoring.SourceManual {
		return Entry{}, fmt.Errorf("%w: the stage does not take manual scores", ErrInvalid)
	}
	if in.Points > scoring.MaxPoints || in.Points < -scoring.MaxPoints {
		return Entry{}, fmt.Errorf("%w: %v", ErrInvalid, scoring.ErrInvalidPoints)
	}
	entrants, err := s.store.Entrants(ctx, c.ID)
	if err != nil {
		return Entry{}, err
	}
	active := false
	for _, e := range entrants {
		if e.ID == in.EntrantID {
			active = e.Status == EntrantActive
		}
	}
	if !active {
		return Entry{}, fmt.Errorf("%w: not an active entrant of this competition", ErrInvalid)
	}
	reason := strings.TrimSpace(in.Reason)
	if utf8.RuneCountInString(reason) > maxReasonRunes {
		return Entry{}, fmt.Errorf("%w: reason is at most %d characters", ErrInvalid, maxReasonRunes)
	}
	return s.store.SetManualScore(ctx, ManualScore{
		CompetitionID: c.ID, StageID: stage.ID, EntrantID: in.EntrantID,
		Points: in.Points, Reason: reason, Actor: lifecycle.ActorID(p.ID),
	})
}

func (s *service) Revoke(ctx context.Context, p authz.Principal, id, entryID uuid.UUID, reason string) (Entry, error) {
	c, err := s.load(ctx, p, id, authz.Update, false)
	if err != nil {
		return Entry{}, err
	}
	if !allowed(c.Status, scoreStatuses) {
		return Entry{}, ErrStatus
	}
	why, err := cleanReason(reason)
	if err != nil {
		return Entry{}, err
	}
	return s.store.Revoke(ctx, c.ID, entryID, lifecycle.ActorID(p.ID), why)
}

func (s *service) Entries(ctx context.Context, p authz.Principal, id uuid.UUID) ([]Entry, error) {
	c, err := s.load(ctx, p, id, authz.Read, false)
	if err != nil {
		return nil, err
	}
	return s.store.Entries(ctx, c.ID)
}

// Leaderboard is the first Stage's live standings, recomputed from the
// ledger on every read: ranked Entrants first, then the active ones
// without a score.
func (s *service) Leaderboard(ctx context.Context, p authz.Principal, id uuid.UUID) (Leaderboard, error) {
	c, err := s.load(ctx, p, id, authz.Read, false)
	if err != nil {
		return Leaderboard{}, err
	}
	stage, err := stageOf(c, nil)
	if err != nil {
		return Leaderboard{}, err
	}
	strategy, ok := scoring.Lookup(stage.Strategy)
	if !ok {
		return Leaderboard{}, fmt.Errorf("competition: stage %s has unknown strategy %q", stage.ID, stage.Strategy)
	}
	entrants, err := s.store.Entrants(ctx, c.ID)
	if err != nil {
		return Leaderboard{}, err
	}
	ledger, err := s.store.Entries(ctx, c.ID)
	if err != nil {
		return Leaderboard{}, err
	}
	in := scoring.Input{Config: stage.Config}
	byID := map[uuid.UUID]Entrant{}
	for _, e := range entrants {
		if e.Status == EntrantActive {
			in.Entrants = append(in.Entrants, e.ID)
			byID[e.ID] = e
		}
	}
	for _, row := range ledger {
		if row.StageID != stage.ID || row.RevokedAt != nil || row.Points == nil {
			continue
		}
		in.Entries = append(in.Entries, scoring.Entry{
			EntrantID: row.EntrantID, Source: row.Source, Value: *row.Points, OccurredAt: row.OccurredAt,
		})
	}
	standings, err := strategy.Standings(in)
	if err != nil {
		return Leaderboard{}, err
	}
	out := Leaderboard{CompetitionID: c.ID, StageID: stage.ID, Strategy: strategy.Key(), Rows: []LeaderboardRow{}}
	ranked := map[uuid.UUID]bool{}
	for _, r := range scoring.Rank(standings) {
		e := byID[r.EntrantID]
		rank, score := r.Rank, r.Score
		out.Rows = append(out.Rows, LeaderboardRow{
			Rank: &rank, EntrantID: e.ID, DisplayName: e.DisplayName, Members: e.Members, Score: &score,
		})
		ranked[e.ID] = true
	}
	for _, e := range entrants {
		if e.Status == EntrantActive && !ranked[e.ID] {
			out.Rows = append(out.Rows, LeaderboardRow{EntrantID: e.ID, DisplayName: e.DisplayName, Members: e.Members})
		}
	}
	return out, nil
}
