package raffle

import (
	"context"
	"encoding/hex"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/certificate"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// Events reads the raffle's Event.
type Events interface {
	Get(ctx context.Context, id uuid.UUID) (event.Event, error)
}

// Attendance reads an Event's attendance as the certificate rule counts it
// (certificate.AttendanceStore): the one count of "who attended how much".
type Attendance interface {
	EventAttendance(ctx context.Context, eventID uuid.UUID) (certificate.Attendance, error)
}

// Service is the raffle's routes behind their authorization.
type Service struct {
	Store      Store
	Events     Events
	Authz      authz.Authorizer
	Attendance Attendance
	// Audit opens the Team audit rows (raffle.draw, raffle.forfeit); nil
	// records none (a test).
	Audit ticket.EventAuditor
	// Teams reads the organizer teams' members; nil excludes no team.
	Teams Teams
	// People reads the door staff's and Co-organizer people's addresses
	// (user.Store.Accounts), to match their guest Tickets; nil matches by
	// account only.
	People func(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]user.Account, error)
	Now    func() time.Time
	// Seed draws a raffle's seed; nil is NewSeed (crypto/rand).
	Seed func() ([SeedSize]byte, error)
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// operable is the Event, for one who may run its raffles: who edits the
// Event (event update) or assigns Tickets (ticket assign), a person, not
// a service account. The door staff do neither. An Event p may not see is
// ErrEventNotFound.
func (s *Service) operable(ctx context.Context, p authz.Principal, eventID uuid.UUID) (event.Event, error) {
	ev, err := s.Events.Get(ctx, eventID)
	if errors.Is(err, event.ErrNotFound) {
		return event.Event{}, ErrEventNotFound
	}
	if err != nil {
		return event.Event{}, err
	}
	if ev.ArchivedAt != nil {
		return event.Event{}, ErrEventNotFound
	}
	if !s.allowed(p, ev) {
		if s.Authz == nil || !event.VisibleTo(s.Authz, &p, ev) {
			return event.Event{}, ErrEventNotFound
		}
		return event.Event{}, ErrForbidden
	}
	return ev, nil
}

func (s *Service) allowed(p authz.Principal, ev event.Event) bool {
	if s.Authz == nil || p.ServiceAccount || p.Product != "" || p.ID == "" {
		return false
	}
	now := s.now()
	return s.Authz.Allow(p, event.AuthzResource(ev, authz.TypeEvent, now), authz.Update) ||
		s.Authz.Allow(p, event.AuthzResource(ev, authz.TypeTicket, now), authz.Assign)
}

// Preview is how many may win under c, and who, masked.
func (s *Service) Preview(ctx context.Context, p authz.Principal, eventID uuid.UUID, c Criteria) (Preview, error) {
	ev, err := s.operable(ctx, p, eventID)
	if err != nil {
		return Preview{}, err
	}
	c, err = c.normalize()
	if err != nil {
		return Preview{}, err
	}
	past := map[uuid.UUID]bool{}
	if c.ExcludePastWinners {
		if past, err = s.Store.PastWinners(ctx, eventID); err != nil {
			return Preview{}, err
		}
	}
	eligible, excluded, err := s.eligible(ctx, ev, c, past)
	if err != nil {
		return Preview{}, err
	}
	out := Preview{Eligible: len(eligible), Excluded: excluded, Candidates: make([]MaskedCandidate, 0, min(len(eligible), MaxCandidates))}
	for _, cand := range eligible {
		if len(out.Candidates) == MaxCandidates {
			out.Truncated = true
			break
		}
		out.Candidates = append(out.Candidates, MaskedCandidate{Name: MaskName(cand.Name), TicketType: cand.TicketType})
	}
	slices.SortStableFunc(out.Candidates, func(a, b MaskedCandidate) int {
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	return out, nil
}

// Draw draws a raffle: the eligible Tickets under the Event's raffle lock,
// a seed from crypto/rand, and the order Order makes of them. Its Team
// audit row (raffle.draw) is opened first: without it nothing is drawn.
func (s *Service) Draw(ctx context.Context, p authz.Principal, eventID uuid.UUID, req Request) (Raffle, error) {
	ev, err := s.operable(ctx, p, eventID)
	if err != nil {
		return Raffle{}, err
	}
	req, err = req.normalize()
	if err != nil {
		return Raffle{}, err
	}
	actor, err := uuid.Parse(p.ID)
	if err != nil {
		return Raffle{}, ErrForbidden
	}
	record, err := s.beginAudit(ctx, p, ev, teamaudit.ActionRaffleDraw)
	if err != nil {
		return Raffle{}, err
	}
	at := s.now().UTC()
	// Who may win is read before the lock; under it, only who won since.
	f, err := s.field(ctx, ev, req.Criteria)
	if err != nil {
		record.Finish(ctx, err, nil)
		return Raffle{}, err
	}
	drawn, err := s.Store.Draw(ctx, eventID, func(pastWinners map[uuid.UUID]bool) (Raffle, error) {
		if !req.Criteria.ExcludePastWinners {
			pastWinners = map[uuid.UUID]bool{}
		}
		eligible, _ := f.pick(pastWinners)
		if len(eligible) < req.Winners {
			return Raffle{}, &TooFewError{Eligible: len(eligible)}
		}
		ids := make([]uuid.UUID, len(eligible))
		for i, cand := range eligible {
			ids[i] = cand.TicketID
		}
		sorted := SortedIDs(ids)
		seed, err := s.seed()
		if err != nil {
			return Raffle{}, err
		}
		alternates := min(req.Alternates, len(sorted)-req.Winners)
		order := Order(sorted, seed, req.Winners+alternates)
		hash := ListHash(sorted)
		r := Raffle{
			ID: uuid.New(), EventID: eventID, Title: req.Title, Criteria: req.Criteria,
			EligibleCount: len(sorted), WinnerCount: req.Winners, AlternateCount: alternates,
			DrawnAt: at, DrawnBy: &Person{ID: actor}, ListHash: hex.EncodeToString(hash[:]),
			Seed: hex.EncodeToString(seed[:]), Algorithm: Algorithm, eligible: sorted,
			Entries: make([]Entry, len(order)),
		}
		for i, id := range order {
			status := StatusWinner
			if i >= req.Winners {
				status = StatusAlternate
			}
			r.Entries[i] = Entry{Position: i + 1, Status: status, TicketID: id}
		}
		return r, nil
	})
	record.Finish(ctx, err, func(after *teamaudit.State) {
		if err == nil {
			after.Raffle = &teamaudit.RaffleInfo{
				ID: drawn.ID.String(), Eligible: drawn.EligibleCount, Winners: drawn.WinnerCount, Alternates: drawn.AlternateCount,
			}
		}
	})
	if err != nil {
		return Raffle{}, err
	}
	return answer(drawn), nil
}

func (s *Service) seed() ([SeedSize]byte, error) {
	if s.Seed != nil {
		return s.Seed()
	}
	return NewSeed()
}

// auditRow is an open Team audit row, or none.
type auditRow struct{ row *ticket.EventAudit }

func (a auditRow) Finish(ctx context.Context, err error, set func(*teamaudit.State)) {
	if a.row != nil {
		a.row.Finish(ctx, err, set)
	}
}

func (s *Service) beginAudit(ctx context.Context, p authz.Principal, ev event.Event, action teamaudit.Action) (auditRow, error) {
	if s.Audit == nil {
		return auditRow{}, nil
	}
	row, err := s.Audit.BeginEventAudit(ctx, p, ev, action)
	if err != nil {
		return auditRow{}, err
	}
	return auditRow{row: row}, nil
}

// List is the Event's raffles, newest first.
func (s *Service) List(ctx context.Context, p authz.Principal, eventID uuid.UUID) ([]Summary, error) {
	if _, err := s.operable(ctx, p, eventID); err != nil {
		return nil, err
	}
	return s.Store.List(ctx, eventID)
}

// Get is one raffle with its entries.
func (s *Service) Get(ctx context.Context, p authz.Principal, eventID, raffleID uuid.UUID) (Raffle, error) {
	if _, err := s.operable(ctx, p, eventID); err != nil {
		return Raffle{}, err
	}
	r, err := s.Store.Get(ctx, eventID, raffleID)
	if err != nil {
		return Raffle{}, err
	}
	return answer(r), nil
}

// Verification is what anyone needs to draw the raffle's order again.
func (s *Service) Verification(ctx context.Context, p authz.Principal, eventID, raffleID uuid.UUID) (Verification, error) {
	if _, err := s.operable(ctx, p, eventID); err != nil {
		return Verification{}, err
	}
	r, err := s.Store.Get(ctx, eventID, raffleID)
	if err != nil {
		return Verification{}, err
	}
	order := make([]uuid.UUID, len(r.Entries))
	for i, e := range r.Entries {
		order[i] = e.TicketID
	}
	ids := r.eligible
	if ids == nil {
		ids = []uuid.UUID{}
	}
	return Verification{Algorithm: r.Algorithm, Seed: r.Seed, ListHash: r.ListHash, TicketIDs: ids, Order: order}, nil
}

// Forfeit gives a winner's place who did not come to the next alternate,
// with the reason kept on the raffle (never in the Team audit row).
func (s *Service) Forfeit(ctx context.Context, p authz.Principal, eventID, raffleID uuid.UUID, position int, rawReason string) (Raffle, error) {
	ev, err := s.operable(ctx, p, eventID)
	if err != nil {
		return Raffle{}, err
	}
	reason, err := normalizeReason(rawReason)
	if err != nil || position < 1 {
		return Raffle{}, ErrInvalid
	}
	actor, err := uuid.Parse(p.ID)
	if err != nil {
		return Raffle{}, ErrForbidden
	}
	before, err := s.Store.Get(ctx, eventID, raffleID)
	if err != nil {
		return Raffle{}, err
	}
	// Refused before a row is opened; the store checks both again.
	if err := forfeitable(before, position); err != nil {
		return Raffle{}, err
	}
	record, err := s.beginAudit(ctx, p, ev, teamaudit.ActionRaffleForfeit)
	if err != nil {
		return Raffle{}, err
	}
	promoted, err := s.Store.Forfeit(ctx, eventID, raffleID, position, reason, actor, s.now().UTC())
	record.Finish(ctx, err, func(after *teamaudit.State) {
		after.Raffle = &teamaudit.RaffleInfo{
			ID: raffleID.String(), Eligible: before.EligibleCount, Winners: before.WinnerCount,
			Alternates: before.AlternateCount, Forfeited: position, Promoted: promoted,
		}
	})
	if err != nil {
		return Raffle{}, err
	}
	r, err := s.Store.Get(ctx, eventID, raffleID)
	if err != nil {
		return Raffle{}, err
	}
	return answer(r), nil
}

// forfeitable reports why position's place cannot be forfeited: it holds
// no winner, or no alternate is left to take it.
func forfeitable(r Raffle, position int) error {
	winner, alternate := false, false
	for _, e := range r.Entries {
		winner = winner || (e.Position == position && e.Status == StatusWinner)
		alternate = alternate || e.Status == StatusAlternate
	}
	switch {
	case !winner:
		return ErrNotWinner
	case !alternate:
		return ErrNoAlternates
	}
	return nil
}

// answer fills what an answer shows of a kept raffle: masked addresses.
func answer(r Raffle) Raffle {
	for i := range r.Entries {
		r.Entries[i].MaskedMail = MaskEmail(r.Entries[i].email)
	}
	return r
}

// excluder is who an exclusion leaves out: accounts, and addresses for
// guest Tickets.
type excluder struct {
	ids    map[uuid.UUID]bool
	emails map[string]bool
}

func newExcluder() excluder {
	return excluder{ids: map[uuid.UUID]bool{}, emails: map[string]bool{}}
}

func (x excluder) add(id uuid.UUID, emails ...string) {
	if id != uuid.Nil {
		x.ids[id] = true
	}
	for _, e := range emails {
		if e = lowerEmail(e); e != "" {
			x.emails[e] = true
		}
	}
}

func (x excluder) matches(c Candidate) bool {
	if c.OwnerID != nil && x.ids[*c.OwnerID] {
		return true
	}
	return c.Email != "" && x.emails[lowerEmail(c.Email)]
}

// people reads ids' accounts' addresses into x; without People, by
// account only.
func (s *Service) people(ctx context.Context, x excluder, ids []uuid.UUID) error {
	for _, id := range ids {
		x.add(id)
	}
	if s.People == nil || len(ids) == 0 {
		return nil
	}
	accounts, err := s.People(ctx, ids)
	if err != nil {
		return err
	}
	for id, a := range accounts {
		x.add(id, a.User.Email, a.User.SchoolEmail)
	}
	return nil
}

func (s *Service) team(ctx context.Context, x excluder, code string) error {
	if s.Teams == nil || code == "" {
		return nil
	}
	members, err := s.Teams.TeamMembers(ctx, code)
	if err != nil {
		return err
	}
	for _, m := range members {
		x.add(m.ID, m.Emails...)
	}
	return nil
}

// field is what the criteria are decided on: the Event's candidates,
// their attendance and who the exclusions leave out. It is read before a
// draw takes the Event's raffle lock (Keycloak's answers among it); only
// the past winners are read under the lock.
type field struct {
	c                                   Criteria
	candidates                          []Candidate
	attended                            map[uuid.UUID]int
	certified                           map[uuid.UUID]bool
	organizers, doorStaff, coOrganizers excluder
}

// eligible are the Event's candidates c lets win, past winners and the
// excluded left out, with the excluded counted.
func (s *Service) eligible(ctx context.Context, ev event.Event, c Criteria, past map[uuid.UUID]bool) ([]Candidate, Excluded, error) {
	f, err := s.field(ctx, ev, c)
	if err != nil {
		return nil, Excluded{}, err
	}
	out, excluded := f.pick(past)
	return out, excluded, nil
}

func (s *Service) field(ctx context.Context, ev event.Event, c Criteria) (field, error) {
	candidates, err := s.Store.Candidates(ctx, ev.ID)
	if err != nil {
		return field{}, err
	}
	var attended map[uuid.UUID]int
	var certified map[uuid.UUID]bool
	if c.MinCounted != nil || c.CertificateEligible {
		a, err := s.Attendance.EventAttendance(ctx, ev.ID)
		if err != nil {
			return field{}, err
		}
		// The one count of who attended how much: the certificate list's
		// and the attendee exports' (certificate.StandingsOf).
		standings := certificate.StandingsOf(ev, a)
		attended = make(map[uuid.UUID]int, len(standings.Tickets))
		certified = make(map[uuid.UUID]bool, len(standings.Tickets))
		for id, st := range standings.Tickets {
			attended[id] = st.Attended
			certified[id] = st.Eligible
		}
	}
	organizers, doorStaff, coOrganizers := newExcluder(), newExcluder(), newExcluder()
	if c.ExcludeOrganizers {
		if err := s.team(ctx, organizers, ev.OwnerTeam); err != nil {
			return field{}, err
		}
	}
	if c.ExcludeDoorStaff {
		if err := s.people(ctx, doorStaff, ev.DoorStaffIDs); err != nil {
			return field{}, err
		}
	}
	if c.ExcludeCoOrganizers {
		for _, team := range ev.CoOrganizers {
			if err := s.team(ctx, coOrganizers, team.Code); err != nil {
				return field{}, err
			}
		}
		if err := s.people(ctx, coOrganizers, ev.CoOrganizerIDs); err != nil {
			return field{}, err
		}
	}
	return field{c: c, candidates: candidates, attended: attended, certified: certified,
		organizers: organizers, doorStaff: doorStaff, coOrganizers: coOrganizers}, nil
}

// pick are the candidates f's criteria let win, past winners and the
// excluded left out, with the excluded counted.
func (f field) pick(past map[uuid.UUID]bool) ([]Candidate, Excluded) {
	c := f.c
	var out []Candidate
	var excluded Excluded
	for _, cand := range f.candidates {
		if !c.holds(cand, f.attended, f.certified) {
			continue
		}
		switch {
		case c.ExcludePastWinners && past[cand.TicketID]:
			excluded.PastWinners++
		case c.ExcludeOrganizers && f.organizers.matches(cand):
			excluded.Organizers++
		case c.ExcludeDoorStaff && f.doorStaff.matches(cand):
			excluded.DoorStaff++
		case c.ExcludeCoOrganizers && f.coOrganizers.matches(cand):
			excluded.CoOrganizers++
		default:
			out = append(out, cand)
		}
	}
	return out, excluded
}

// holds reports whether cand meets c's conditions (its exclusions aside).
func (c Criteria) holds(cand Candidate, attended map[uuid.UUID]int, certified map[uuid.UUID]bool) bool {
	if c.MinCounted != nil && attended[cand.TicketID] < *c.MinCounted {
		return false
	}
	if c.CertificateEligible && !certified[cand.TicketID] {
		return false
	}
	if c.TicketType != "" && cand.TicketType != c.TicketType {
		return false
	}
	if len(c.SessionIDs) > 0 {
		found := 0
		for _, id := range c.SessionIDs {
			if slices.Contains(cand.Sessions, id) {
				found++
			}
		}
		if (c.SessionMatch == MatchAny && found == 0) || (c.SessionMatch != MatchAny && found < len(c.SessionIDs)) {
			return false
		}
	}
	switch c.EnteredOn {
	case "":
	case EnteredAny:
		if len(cand.Entered) == 0 {
			return false
		}
	default:
		if !slices.Contains(cand.Entered, c.EnteredOn) {
			return false
		}
	}
	return true
}
