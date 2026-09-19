package ticket

import (
	"context"
	"errors"
	"slices"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

type Service interface {
	Apply(ctx context.Context, p authz.Principal, eventID uuid.UUID) (Ticket, error)
	ApplyForOther(ctx context.Context, p authz.Principal, eventID, userID uuid.UUID) (Ticket, error)
	ListAssignableUsers(ctx context.Context, p authz.Principal, eventID uuid.UUID, query string) ([]PersonSummary, error)
	ListDoorEvents(ctx context.Context, p authz.Principal) ([]event.Resource, error)
	SearchDoorAttendees(ctx context.Context, p authz.Principal, eventID uuid.UUID, query string) ([]DoorAttendee, error)
	ApplyGuest(ctx context.Context, eventID uuid.UUID, g GuestInfo) (Ticket, error)
	Mine(ctx context.Context, p authz.Principal) ([]Ticket, error)
	ListByEvent(ctx context.Context, p authz.Principal, eventID uuid.UUID) ([]Ticket, error)
	Get(ctx context.Context, p authz.Principal, id uuid.UUID) (Ticket, error)
	GetByUserEvent(ctx context.Context, p authz.Principal, userID, eventID uuid.UUID) (Ticket, error)
	ListQuery(ctx context.Context, p authz.Principal, email string, userID *uuid.UUID) ([]Ticket, error)
	CheckIn(ctx context.Context, p authz.Principal, ticketID, sessionID uuid.UUID) (CheckIn, error)
	CheckInMe(ctx context.Context, p authz.Principal, sessionID uuid.UUID) (CheckIn, error)
	CheckInGuest(ctx context.Context, sessionID uuid.UUID, email string) (CheckIn, error)
	CheckInUser(ctx context.Context, p authz.Principal, sessionID, userID uuid.UUID) (CheckIn, error)
	ResolveAndCheckIn(ctx context.Context, p authz.Principal, sessionID uuid.UUID, target DoorCheckInTarget) (DoorCheckIn, error)
	DoorActivity(ctx context.Context, p authz.Principal, sessionID uuid.UUID) (DoorActivity, error)
}

type TeamReader interface {
	GetGroup(ctx context.Context, idOrPath string) (identity.Group, error)
}

type PersonReader interface {
	GetUser(ctx context.Context, id uuid.UUID) (identity.Person, error)
}

type PersonDirectory interface {
	PersonReader
	SearchUsers(ctx context.Context, query string, limit int) ([]identity.Person, error)
}

type limitedUserSearcher interface {
	SearchLimit(ctx context.Context, query string, limit int) ([]user.User, error)
}

type service struct {
	tickets   Store
	events    event.Store
	users     user.Store
	people    PersonReader
	directory PersonDirectory
	teams     TeamReader
	authz     authz.Authorizer
}

func NewService(tickets Store, events event.Store, az authz.Authorizer, extras ...any) Service {
	s := &service{tickets: tickets, events: events, authz: az}
	for _, extra := range extras {
		if d, ok := extra.(identity.Directory); ok {
			s.people = d
			s.directory = d
			s.teams = d
		}
		if v, ok := extra.(user.Store); ok {
			s.users = v
		}
		if v, ok := extra.(TeamReader); ok && s.teams == nil {
			s.teams = v
		}
		if v, ok := extra.(PersonReader); ok && s.people == nil {
			s.people = v
		}
	}
	return s
}

const assignableUserLimit = 20

func (s *service) personSummary(ctx context.Context, person identity.Person, found bool) (PersonSummary, bool) {
	if s.users != nil {
		if shadow, err := s.users.Get(ctx, person.ID); err == nil {
			found = true
			if shadow.Email != "" {
				person.Email = shadow.Email
			}
			person.FirstName = shadow.FirstName
			person.LastName = shadow.LastName
		}
	}
	return PersonSummary{ID: person.ID, Email: person.Email, FirstName: person.FirstName, LastName: person.LastName}, found
}

func (s *service) ListAssignableUsers(ctx context.Context, p authz.Principal, eventID uuid.UUID, query string) ([]PersonSummary, error) {
	ev, err := s.events.Get(ctx, eventID)
	if err != nil {
		if errors.Is(err, event.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if !s.authz.Allow(p, authz.Resource{Type: authz.TypeTicket, OwnerTeam: ev.OwnerTeam}, authz.Assign) {
		return nil, ErrForbidden
	}
	query = strings.TrimSpace(query)
	if len([]rune(query)) < 2 {
		return []PersonSummary{}, nil
	}
	if s.directory == nil && s.users == nil {
		return []PersonSummary{}, nil
	}
	people := make([]identity.Person, 0, assignableUserLimit)
	var directoryErr error
	if s.directory != nil {
		people, directoryErr = s.directory.SearchUsers(ctx, query, assignableUserLimit)
	}
	byID := make(map[uuid.UUID]PersonSummary)
	for _, person := range people {
		summary, _ := s.personSummary(ctx, person, true)
		if personSummaryMatches(summary, query) {
			byID[summary.ID] = summary
		}
	}
	var shadowErr error
	if s.users != nil {
		var shadows []user.User
		if searcher, ok := s.users.(limitedUserSearcher); ok {
			shadows, shadowErr = searcher.SearchLimit(ctx, query, assignableUserLimit)
		} else {
			shadows, shadowErr = s.users.Search(ctx, query)
		}
		for _, shadow := range shadows {
			summary := PersonSummary{
				ID: shadow.ID, Email: shadow.Email, FirstName: shadow.FirstName, LastName: shadow.LastName,
			}
			if personSummaryMatches(summary, query) {
				byID[shadow.ID] = summary
			}
		}
	}
	if directoryErr != nil && shadowErr != nil {
		return nil, directoryErr
	}
	if directoryErr != nil && s.users == nil {
		return nil, directoryErr
	}
	if shadowErr != nil && s.directory == nil {
		return nil, shadowErr
	}
	out := make([]PersonSummary, 0, len(byID))
	for _, summary := range byID {
		out = append(out, summary)
	}
	sort.Slice(out, func(i, j int) bool {
		left := strings.ToLower(out[i].LastName + "\x00" + out[i].FirstName + "\x00" + out[i].Email)
		right := strings.ToLower(out[j].LastName + "\x00" + out[j].FirstName + "\x00" + out[j].Email)
		return left < right
	})
	if len(out) > assignableUserLimit {
		out = out[:assignableUserLimit]
	}
	return out, nil
}

func personSummaryMatches(summary PersonSummary, query string) bool {
	needle := strings.ToLower(strings.TrimSpace(query))
	if needle == "" {
		return false
	}
	values := []string{
		summary.Email,
		summary.FirstName,
		summary.LastName,
		strings.TrimSpace(summary.FirstName + " " + summary.LastName),
	}
	for _, value := range values {
		if strings.Contains(strings.ToLower(value), needle) {
			return true
		}
	}
	return false
}

func (s *service) withEvent(ctx context.Context, t Ticket) Ticket {
	if t.OwnerID != nil {
		if s.users != nil {
			if shadow, err := s.users.Get(ctx, *t.OwnerID); err == nil {
				summary := PersonSummary{
					ID: shadow.ID, Email: shadow.Email, FirstName: shadow.FirstName, LastName: shadow.LastName,
				}
				t.Owner = &summary
			}
		}
		if t.Owner == nil && s.people != nil {
			if directoryPerson, err := s.people.GetUser(ctx, *t.OwnerID); err == nil {
				summary := PersonSummary{
					ID: directoryPerson.ID, Email: directoryPerson.Email,
					FirstName: directoryPerson.FirstName, LastName: directoryPerson.LastName,
				}
				t.Owner = &summary
			}
		}
	}
	ev, err := s.events.Get(ctx, t.EventID)
	if err != nil {
		return t
	}
	res := ev.Resource()
	t.Event = &res
	return t
}

func (s *service) withEvents(ctx context.Context, tickets []Ticket) []Ticket {
	out := make([]Ticket, len(tickets))
	for i, t := range tickets {
		out[i] = s.withEvent(ctx, t)
	}
	return out
}

func (s *service) ListDoorEvents(ctx context.Context, p authz.Principal) ([]event.Resource, error) {
	events, err := s.events.List(ctx, "", false)
	if err != nil {
		return nil, err
	}
	out := make([]event.Resource, 0)
	for _, ev := range events {
		if s.canUseDoor(ctx, p, ev) {
			out = append(out, ev.Resource())
		}
	}
	return out, nil
}

func valuesContainDoorQuery(values []string, query string) bool {
	needle := normalizedDoorValue(query)
	if needle == "" {
		return false
	}
	for _, value := range values {
		if strings.Contains(normalizedDoorValue(value), needle) {
			return true
		}
	}
	return false
}

func (s *service) SearchDoorAttendees(
	ctx context.Context,
	p authz.Principal,
	eventID uuid.UUID,
	query string,
) ([]DoorAttendee, error) {
	ev, err := s.events.Get(ctx, eventID)
	if err != nil {
		if errors.Is(err, event.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if !s.canUseDoor(ctx, p, ev) {
		return nil, ErrForbidden
	}
	query = strings.TrimSpace(query)
	if len([]rune(query)) < 2 {
		return []DoorAttendee{}, nil
	}
	if store, ok := s.tickets.(DoorStore); ok {
		return s.searchStoredDoorAttendees(ctx, store, eventID, query)
	}
	listed, err := s.tickets.ListByEvent(ctx, eventID)
	if err != nil {
		return nil, err
	}
	out := make([]DoorAttendee, 0)
	for _, candidate := range listed {
		name, values, found := s.localDoorTicketIdentity(ctx, candidate)
		email := ""
		if candidate.TicketType == Guest {
			email = candidate.GuestEmail
		} else if candidate.OwnerID != nil {
			if found && len(values) > 0 {
				email = values[0]
			}
		}
		if !found || !valuesContainDoorQuery(values, query) {
			continue
		}
		row := DoorAttendee{Name: name, Email: email}
		if candidate.OwnerID != nil {
			personID := *candidate.OwnerID
			row.PersonID = &personID
		}
		out = append(out, row)
	}
	if len(out) == 0 && s.directory != nil {
		people, _ := s.directory.SearchUsers(ctx, query, assignableUserLimit)
		directoryMatches := make(map[uuid.UUID]PersonSummary)
		for _, person := range people {
			summary, _ := s.personSummary(ctx, person, true)
			_, values := summaryDoorIdentity(summary)
			if valuesContainDoorQuery(values, query) {
				directoryMatches[summary.ID] = summary
			}
		}
		for _, candidate := range listed {
			if candidate.OwnerID == nil {
				continue
			}
			summary, ok := directoryMatches[*candidate.OwnerID]
			if !ok {
				continue
			}
			name, _ := summaryDoorIdentity(summary)
			personID := *candidate.OwnerID
			out = append(out, DoorAttendee{
				PersonID: &personID, Name: name, Email: summary.Email,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].Name+"\x00"+out[i].Email) < strings.ToLower(out[j].Name+"\x00"+out[j].Email)
	})
	if len(out) > assignableUserLimit {
		out = out[:assignableUserLimit]
	}
	return out, nil
}

func (s *service) searchStoredDoorAttendees(
	ctx context.Context,
	store DoorStore,
	eventID uuid.UUID,
	query string,
) ([]DoorAttendee, error) {
	rows, err := store.SearchDoorTickets(ctx, eventID, query, false, assignableUserLimit)
	if err != nil {
		return nil, err
	}
	out := make([]DoorAttendee, 0, len(rows))
	for _, candidate := range rows {
		row := DoorAttendee{Name: candidate.Name, Email: candidate.Email}
		if candidate.Ticket.OwnerID != nil {
			personID := *candidate.Ticket.OwnerID
			row.PersonID = &personID
		}
		out = append(out, row)
	}
	if len(out) > 0 || s.directory == nil {
		return out, nil
	}
	people, _ := s.directory.SearchUsers(ctx, query, assignableUserLimit)
	directoryMatches := make(map[uuid.UUID]PersonSummary)
	ownerIDs := make([]uuid.UUID, 0, len(people))
	for _, person := range people {
		summary := PersonSummary{
			ID: person.ID, Email: person.Email, FirstName: person.FirstName, LastName: person.LastName,
		}
		directoryMatches[summary.ID] = summary
		ownerIDs = append(ownerIDs, summary.ID)
	}
	tickets, err := store.DoorTicketsByOwners(ctx, eventID, ownerIDs)
	if err != nil {
		return nil, err
	}
	for _, candidate := range tickets {
		if candidate.Ticket.OwnerID == nil {
			continue
		}
		summary, found := directoryMatches[*candidate.Ticket.OwnerID]
		if !found {
			continue
		}
		name, email := candidate.Name, candidate.Email
		if candidate.Found {
			if !valuesContainDoorQuery([]string{email, name, candidate.Ticket.OwnerID.String()}, query) {
				continue
			}
		} else {
			var values []string
			name, values = summaryDoorIdentity(summary)
			if !valuesContainDoorQuery(values, query) {
				continue
			}
			email = summary.Email
		}
		personID := *candidate.Ticket.OwnerID
		out = append(out, DoorAttendee{PersonID: &personID, Name: name, Email: email})
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].Name+"\x00"+out[i].Email) < strings.ToLower(out[j].Name+"\x00"+out[j].Email)
	})
	if len(out) > assignableUserLimit {
		out = out[:assignableUserLimit]
	}
	return out, nil
}

func (s *service) canRead(ctx context.Context, p authz.Principal, t Ticket) bool {
	if t.OwnerID != nil && p.ID == t.OwnerID.String() {
		return true
	}
	ev, err := s.events.Get(ctx, t.EventID)
	if err != nil {
		return false
	}
	return s.authz.Allow(p, authz.Resource{Type: authz.TypeTicket, OwnerTeam: ev.OwnerTeam}, authz.Read)
}

func (s *service) Apply(ctx context.Context, p authz.Principal, eventID uuid.UUID) (Ticket, error) {
	if !s.authz.Allow(p, authz.Resource{Type: authz.TypeTicket}, authz.Create) {
		return Ticket{}, ErrForbidden
	}
	if _, err := s.events.Get(ctx, eventID); err != nil {
		if errors.Is(err, event.ErrNotFound) {
			return Ticket{}, ErrNotFound
		}
		return Ticket{}, err
	}
	ownerID, err := uuid.Parse(p.ID)
	if err != nil {
		return Ticket{}, ErrInvalid
	}
	return s.applyRegistered(ctx, eventID, ownerID)
}

func (s *service) ApplyForOther(ctx context.Context, p authz.Principal, eventID, userID uuid.UUID) (Ticket, error) {
	ev, err := s.events.Get(ctx, eventID)
	if err != nil {
		if errors.Is(err, event.ErrNotFound) {
			return Ticket{}, ErrNotFound
		}
		return Ticket{}, err
	}
	if !s.authz.Allow(p, authz.Resource{Type: authz.TypeTicket, OwnerTeam: ev.OwnerTeam}, authz.Assign) {
		return Ticket{}, ErrForbidden
	}
	if userID == uuid.Nil {
		return Ticket{}, ErrInvalid
	}
	if err := s.resolveApplyTarget(ctx, userID); err != nil {
		return Ticket{}, err
	}
	return s.applyRegistered(ctx, eventID, userID)
}

func (s *service) resolveApplyTarget(ctx context.Context, userID uuid.UUID) error {
	if s.users != nil {
		if _, err := s.users.Get(ctx, userID); err == nil {
			return nil
		} else if !errors.Is(err, user.ErrNotFound) {
			return err
		}
	}
	people := s.people
	if people == nil {
		if p, ok := any(s.teams).(PersonReader); ok {
			people = p
		}
	}
	if people == nil {
		return ErrNotFound
	}
	person, err := people.GetUser(ctx, userID)
	if err != nil {
		if errors.Is(err, identity.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	if s.users == nil {
		return nil
	}
	_, _, err = user.NewService(s.users).Ensure(ctx, userID, user.Profile{
		Email:       person.Email,
		FirstName:   person.FirstName,
		LastName:    person.LastName,
		Username:    person.Username,
		SchoolEmail: person.SchoolEmail,
		SkyNumber:   person.SkyNumber,
	})
	return err
}

func (s *service) applyRegistered(ctx context.Context, eventID, ownerID uuid.UUID) (Ticket, error) {
	exists, err := s.tickets.ExistsOwnerEvent(ctx, ownerID, eventID)
	if err != nil {
		return Ticket{}, err
	}
	if exists {
		return Ticket{}, ErrConflict
	}
	created, err := s.tickets.Create(ctx, Ticket{
		EventID:    eventID,
		TicketType: Registered,
		OwnerID:    &ownerID,
	})
	if err != nil {
		return Ticket{}, err
	}
	return s.withEvent(ctx, created), nil
}

func normalizeGuest(g GuestInfo) GuestInfo {
	g.FirstName = strings.TrimSpace(g.FirstName)
	g.LastName = strings.TrimSpace(g.LastName)
	g.Email = strings.ToLower(strings.TrimSpace(g.Email))
	g.PhoneNumber = strings.TrimSpace(g.PhoneNumber)
	return g
}

func (s *service) ApplyGuest(ctx context.Context, eventID uuid.UUID, g GuestInfo) (Ticket, error) {
	g = normalizeGuest(g)
	if g.FirstName == "" || g.LastName == "" || g.Email == "" {
		return Ticket{}, ErrInvalid
	}
	if _, err := s.events.Get(ctx, eventID); err != nil {
		if errors.Is(err, event.ErrNotFound) {
			return Ticket{}, ErrNotFound
		}
		return Ticket{}, err
	}
	existing, err := s.tickets.GetByGuestEvent(ctx, g.Email, eventID)
	if err == nil {
		existing.GuestFirstName = g.FirstName
		existing.GuestLastName = g.LastName
		existing.GuestEmail = g.Email
		if g.PhoneNumber != "" {
			existing.GuestPhoneNumber = g.PhoneNumber
		}
		updated, err := s.tickets.Update(ctx, existing)
		if err != nil {
			return Ticket{}, err
		}
		return s.withEvent(ctx, updated), nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Ticket{}, err
	}
	created, err := s.tickets.Create(ctx, Ticket{
		EventID:          eventID,
		TicketType:       Guest,
		GuestFirstName:   g.FirstName,
		GuestLastName:    g.LastName,
		GuestEmail:       g.Email,
		GuestPhoneNumber: g.PhoneNumber,
	})
	if err != nil {
		return Ticket{}, err
	}
	return s.withEvent(ctx, created), nil
}

func (s *service) Mine(ctx context.Context, p authz.Principal) ([]Ticket, error) {
	if !s.authz.Allow(p, authz.Resource{Type: authz.TypeTicket}, authz.ReadMe) {
		return nil, ErrForbidden
	}
	ownerID, err := uuid.Parse(p.ID)
	if err != nil {
		return nil, ErrInvalid
	}
	tickets, err := s.tickets.ListByOwner(ctx, ownerID)
	if err != nil {
		return nil, err
	}
	return s.withEvents(ctx, tickets), nil
}

func (s *service) ListByEvent(ctx context.Context, p authz.Principal, eventID uuid.UUID) ([]Ticket, error) {
	ev, err := s.events.Get(ctx, eventID)
	if err != nil {
		if errors.Is(err, event.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if !s.authz.Allow(p, authz.Resource{Type: authz.TypeTicket, OwnerTeam: ev.OwnerTeam}, authz.Read) {
		return nil, ErrForbidden
	}
	tickets, err := s.tickets.ListByEvent(ctx, eventID)
	if err != nil {
		return nil, err
	}
	return s.withEvents(ctx, tickets), nil
}

func (s *service) Get(ctx context.Context, p authz.Principal, id uuid.UUID) (Ticket, error) {
	t, err := s.tickets.Get(ctx, id)
	if err != nil {
		return Ticket{}, err
	}
	if !s.canRead(ctx, p, t) {
		return Ticket{}, ErrForbidden
	}
	return s.withEvent(ctx, t), nil
}

func (s *service) GetByUserEvent(ctx context.Context, p authz.Principal, userID, eventID uuid.UUID) (Ticket, error) {
	t, err := s.tickets.GetByOwnerEvent(ctx, userID, eventID)
	if err != nil {
		return Ticket{}, err
	}
	if !s.canRead(ctx, p, t) {
		return Ticket{}, ErrForbidden
	}
	return s.withEvent(ctx, t), nil
}

func (s *service) ListQuery(ctx context.Context, p authz.Principal, email string, userID *uuid.UUID) ([]Ticket, error) {
	email = strings.TrimSpace(email)
	if email == "" && userID == nil {
		return nil, ErrInvalid
	}

	var byUser []Ticket
	if userID != nil {
		listed, err := s.tickets.ListByOwner(ctx, *userID)
		if err != nil {
			return nil, err
		}
		byUser = listed
	}

	ownEmail := false
	var byEmail []Ticket
	if email != "" {
		if s.users != nil {
			found, err := s.users.FindByEmail(ctx, email)
			if err != nil {
				return nil, err
			}
			for _, u := range found {
				if p.ID == u.ID.String() {
					ownEmail = true
				}
				listed, err := s.tickets.ListByOwner(ctx, u.ID)
				if err != nil {
					return nil, err
				}
				byEmail = append(byEmail, listed...)
			}
		}
		guests, err := s.tickets.ListByGuestEmail(ctx, email)
		if err != nil {
			return nil, err
		}
		byEmail = append(byEmail, guests...)
	}

	var tickets []Ticket
	switch {
	case userID != nil && email != "":
		tickets = intersectTickets(byUser, byEmail)
	case userID != nil:
		tickets = byUser
	default:
		tickets = byEmail
	}

	seen := map[uuid.UUID]struct{}{}
	uniq := make([]Ticket, 0, len(tickets))
	for _, t := range tickets {
		if _, ok := seen[t.ID]; ok {
			continue
		}
		seen[t.ID] = struct{}{}
		uniq = append(uniq, t)
	}

	ownUser := userID != nil && p.ID == userID.String()
	own := ownUser
	if userID == nil && ownEmail {
		own = true
	}

	if own {
		return s.withEvents(ctx, uniq), nil
	}
	visible := make([]Ticket, 0, len(uniq))
	for _, t := range uniq {
		if s.canRead(ctx, p, t) {
			visible = append(visible, t)
		}
	}
	if len(visible) == 0 && !s.authz.Allow(p, authz.Resource{Type: authz.TypeTicket}, authz.Read) && !hasLeaderGroup(p) {
		return nil, ErrForbidden
	}
	return s.withEvents(ctx, visible), nil
}

func intersectTickets(a, b []Ticket) []Ticket {
	inB := map[uuid.UUID]struct{}{}
	for _, t := range b {
		inB[t.ID] = struct{}{}
	}
	out := make([]Ticket, 0)
	for _, t := range a {
		if _, ok := inB[t.ID]; ok {
			out = append(out, t)
		}
	}
	return out
}

func hasLeaderGroup(p authz.Principal) bool {
	for _, g := range p.Groups {
		if strings.Contains(g, "/LIDERLER") || strings.Contains(g, "/KOORDINATORLER") {
			return true
		}
		if strings.HasSuffix(g, "/YK") || strings.Contains(g, "/YK/") {
			return true
		}
		if strings.HasSuffix(g, "/DK") || strings.Contains(g, "/DK/") {
			return true
		}
		if strings.HasSuffix(g, "/ADMIN") || strings.Contains(g, "/ADMIN/") {
			return true
		}
	}
	return false
}

func (s *service) CheckIn(ctx context.Context, p authz.Principal, ticketID, sessionID uuid.UUID) (CheckIn, error) {
	t, err := s.tickets.Get(ctx, ticketID)
	if err != nil {
		return CheckIn{}, err
	}
	ev, err := s.events.Get(ctx, t.EventID)
	if err != nil {
		if errors.Is(err, event.ErrNotFound) {
			return CheckIn{}, ErrNotFound
		}
		return CheckIn{}, err
	}
	if !s.canUseDoor(ctx, p, ev) {
		return CheckIn{}, ErrForbidden
	}
	return s.addSessionCheckIn(ctx, t, sessionID)
}

func (s *service) addSessionCheckIn(ctx context.Context, t Ticket, sessionID uuid.UUID) (CheckIn, error) {
	sess, err := s.events.GetSession(ctx, sessionID)
	if err != nil {
		if errors.Is(err, event.ErrNotFound) {
			return CheckIn{}, ErrNotFound
		}
		return CheckIn{}, err
	}
	day, err := s.events.GetDay(ctx, sess.EventDayID)
	if err != nil {
		if errors.Is(err, event.ErrNotFound) {
			return CheckIn{}, ErrNotFound
		}
		return CheckIn{}, err
	}
	if day.EventID != t.EventID {
		return CheckIn{}, ErrInvalid
	}
	dup, err := s.tickets.HasCheckIn(ctx, t.ID, sessionID)
	if err != nil {
		return CheckIn{}, err
	}
	if dup {
		return CheckIn{}, ErrConflict
	}
	return s.tickets.AddCheckIn(ctx, CheckIn{TicketID: t.ID, SessionID: sessionID, EventDayID: sess.EventDayID})
}

func (s *service) ticketEventID(ctx context.Context, sessionID uuid.UUID) (uuid.UUID, error) {
	sess, err := s.events.GetSession(ctx, sessionID)
	if err != nil {
		if errors.Is(err, event.ErrNotFound) {
			return uuid.Nil, ErrNotFound
		}
		return uuid.Nil, err
	}
	day, err := s.events.GetDay(ctx, sess.EventDayID)
	if err != nil {
		if errors.Is(err, event.ErrNotFound) {
			return uuid.Nil, ErrNotFound
		}
		return uuid.Nil, err
	}
	return day.EventID, nil
}

func (s *service) CheckInMe(ctx context.Context, p authz.Principal, sessionID uuid.UUID) (CheckIn, error) {
	ownerID, err := uuid.Parse(p.ID)
	if err != nil {
		return CheckIn{}, ErrInvalid
	}
	eventID, err := s.ticketEventID(ctx, sessionID)
	if err != nil {
		return CheckIn{}, err
	}
	t, err := s.tickets.GetByOwnerEvent(ctx, ownerID, eventID)
	if err != nil {
		return CheckIn{}, err
	}
	return s.addSessionCheckIn(ctx, t, sessionID)
}

func (s *service) CheckInGuest(ctx context.Context, sessionID uuid.UUID, email string) (CheckIn, error) {
	email = strings.TrimSpace(email)
	if email == "" {
		return CheckIn{}, ErrInvalid
	}
	eventID, err := s.ticketEventID(ctx, sessionID)
	if err != nil {
		return CheckIn{}, err
	}
	listed, err := s.tickets.ListByGuestEmail(ctx, email)
	if err != nil {
		return CheckIn{}, err
	}
	for _, t := range listed {
		if t.EventID == eventID {
			return s.addSessionCheckIn(ctx, t, sessionID)
		}
	}
	return CheckIn{}, ErrNotFound
}

func (s *service) CheckInUser(ctx context.Context, p authz.Principal, sessionID, userID uuid.UUID) (CheckIn, error) {
	eventID, err := s.ticketEventID(ctx, sessionID)
	if err != nil {
		return CheckIn{}, err
	}
	t, err := s.tickets.GetByOwnerEvent(ctx, userID, eventID)
	if err != nil {
		return CheckIn{}, err
	}
	return s.CheckIn(ctx, p, t.ID, sessionID)
}

func normalizedDoorValue(value string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(value))), " ")
}

func summaryDoorIdentity(summary PersonSummary) (string, []string) {
	name := strings.TrimSpace(strings.Join([]string{summary.FirstName, summary.LastName}, " "))
	if name == "" {
		name = strings.TrimSpace(summary.Email)
	}
	return name, []string{summary.Email, name, summary.ID.String()}
}

func (s *service) localDoorTicketIdentity(ctx context.Context, t Ticket) (string, []string, bool) {
	if t.TicketType == Guest {
		name := strings.TrimSpace(strings.Join([]string{t.GuestFirstName, t.GuestLastName}, " "))
		if name == "" {
			name = strings.TrimSpace(t.GuestEmail)
		}
		return name, []string{t.GuestEmail, name}, true
	}
	if t.OwnerID == nil {
		return "", nil, false
	}
	if s.users != nil {
		if shadow, err := s.users.Get(ctx, *t.OwnerID); err == nil {
			name, values := summaryDoorIdentity(PersonSummary{
				ID: shadow.ID, Email: shadow.Email, FirstName: shadow.FirstName, LastName: shadow.LastName,
			})
			return name, values, true
		}
	}
	if t.Owner != nil {
		name, values := summaryDoorIdentity(*t.Owner)
		return name, values, true
	}
	return t.OwnerID.String(), []string{t.OwnerID.String()}, false
}

func (s *service) doorTicketIdentity(ctx context.Context, t Ticket) (string, []string) {
	name, values, found := s.localDoorTicketIdentity(ctx, t)
	if found || t.OwnerID == nil || s.people == nil {
		return name, values
	}
	person, err := s.people.GetUser(ctx, *t.OwnerID)
	if err != nil {
		return name, values
	}
	return summaryDoorIdentity(PersonSummary{
		ID: person.ID, Email: person.Email, FirstName: person.FirstName, LastName: person.LastName,
	})
}

func (s *service) authorizedDoorEvent(
	ctx context.Context,
	p authz.Principal,
	sessionID uuid.UUID,
) (event.Event, error) {
	eventID, err := s.ticketEventID(ctx, sessionID)
	if err != nil {
		return event.Event{}, err
	}
	ev, err := s.events.Get(ctx, eventID)
	if err != nil {
		if errors.Is(err, event.ErrNotFound) {
			return event.Event{}, ErrNotFound
		}
		return event.Event{}, err
	}
	if !s.canUseDoor(ctx, p, ev) {
		return event.Event{}, ErrForbidden
	}
	return ev, nil
}

func (s *service) ResolveAndCheckIn(
	ctx context.Context,
	p authz.Principal,
	sessionID uuid.UUID,
	target DoorCheckInTarget,
) (DoorCheckIn, error) {
	ev, err := s.authorizedDoorEvent(ctx, p, sessionID)
	if err != nil {
		return DoorCheckIn{}, err
	}
	query := normalizedDoorValue(target.Query)
	if target.PersonID == nil && query == "" {
		return DoorCheckIn{}, ErrInvalid
	}
	if store, ok := s.tickets.(DoorStore); ok {
		return s.resolveStoredDoorCheckIn(ctx, store, ev, sessionID, target, query)
	}
	listed, err := s.tickets.ListByEvent(ctx, ev.ID)
	if err != nil {
		return DoorCheckIn{}, err
	}
	type match struct {
		ticket Ticket
		name   string
	}
	matches := make([]match, 0, 1)
	for _, candidate := range listed {
		if target.PersonID != nil {
			if candidate.OwnerID != nil && *candidate.OwnerID == *target.PersonID {
				name, _ := s.doorTicketIdentity(ctx, candidate)
				matches = append(matches, match{ticket: candidate, name: name})
			}
			continue
		}
		name, values, found := s.localDoorTicketIdentity(ctx, candidate)
		if !found {
			continue
		}
		for _, value := range values {
			if normalizedDoorValue(value) == query {
				matches = append(matches, match{ticket: candidate, name: name})
				break
			}
		}
	}
	if len(matches) == 0 && target.PersonID == nil && query != "" && s.directory != nil {
		people, _ := s.directory.SearchUsers(ctx, target.Query, assignableUserLimit)
		directoryMatches := make(map[uuid.UUID]PersonSummary)
		for _, person := range people {
			summary, _ := s.personSummary(ctx, person, true)
			_, values := summaryDoorIdentity(summary)
			for _, value := range values {
				if normalizedDoorValue(value) == query {
					directoryMatches[summary.ID] = summary
					break
				}
			}
		}
		for _, candidate := range listed {
			if candidate.OwnerID == nil {
				continue
			}
			if summary, ok := directoryMatches[*candidate.OwnerID]; ok {
				name, _ := summaryDoorIdentity(summary)
				matches = append(matches, match{ticket: candidate, name: name})
			}
		}
	}
	if len(matches) == 0 {
		return DoorCheckIn{}, ErrNotFound
	}
	if len(matches) > 1 {
		return DoorCheckIn{}, ErrAmbiguous
	}
	ci, err := s.addSessionCheckIn(ctx, matches[0].ticket, sessionID)
	if err != nil {
		return DoorCheckIn{}, err
	}
	return DoorCheckIn{
		ID: ci.ID, TicketID: ci.TicketID, SessionID: ci.SessionID, EventDayID: ci.EventDayID,
		PersonName: matches[0].name, CreatedAt: ci.CreatedAt,
	}, nil
}

func (s *service) resolveStoredDoorCheckIn(
	ctx context.Context,
	store DoorStore,
	ev event.Event,
	sessionID uuid.UUID,
	target DoorCheckInTarget,
	query string,
) (DoorCheckIn, error) {
	var rows []DoorTicketIdentity
	var err error
	if target.PersonID != nil {
		rows, err = store.DoorTicketsByOwners(ctx, ev.ID, []uuid.UUID{*target.PersonID})
	} else {
		rows, err = store.SearchDoorTickets(ctx, ev.ID, query, true, 2)
	}
	if err != nil {
		return DoorCheckIn{}, err
	}
	if len(rows) == 0 && target.PersonID == nil && s.directory != nil {
		people, _ := s.directory.SearchUsers(ctx, target.Query, assignableUserLimit)
		summaries := make(map[uuid.UUID]PersonSummary)
		ownerIDs := make([]uuid.UUID, 0, len(people))
		for _, person := range people {
			summary := PersonSummary{
				ID: person.ID, Email: person.Email, FirstName: person.FirstName, LastName: person.LastName,
			}
			_, values := summaryDoorIdentity(summary)
			for _, value := range values {
				if normalizedDoorValue(value) == query {
					summaries[summary.ID] = summary
					ownerIDs = append(ownerIDs, summary.ID)
					break
				}
			}
		}
		rows, err = store.DoorTicketsByOwners(ctx, ev.ID, ownerIDs)
		if err != nil {
			return DoorCheckIn{}, err
		}
		for i := range rows {
			if rows[i].Ticket.OwnerID == nil {
				continue
			}
			summary, found := summaries[*rows[i].Ticket.OwnerID]
			if !found {
				continue
			}
			if rows[i].Found {
				values := []string{rows[i].Email, rows[i].Name, rows[i].Ticket.OwnerID.String()}
				matched := false
				for _, value := range values {
					if normalizedDoorValue(value) == query {
						matched = true
						break
					}
				}
				if !matched {
					rows[i].Ticket.ID = uuid.Nil
				}
			} else {
				rows[i].Name, _ = summaryDoorIdentity(summary)
			}
		}
		rows = slices.DeleteFunc(rows, func(row DoorTicketIdentity) bool {
			return row.Ticket.ID == uuid.Nil
		})
	}
	if len(rows) == 0 {
		return DoorCheckIn{}, ErrNotFound
	}
	if len(rows) > 1 {
		return DoorCheckIn{}, ErrAmbiguous
	}
	ci, err := s.addSessionCheckIn(ctx, rows[0].Ticket, sessionID)
	if err != nil {
		return DoorCheckIn{}, err
	}
	return DoorCheckIn{
		ID: ci.ID, TicketID: ci.TicketID, SessionID: ci.SessionID, EventDayID: ci.EventDayID,
		PersonName: rows[0].Name, CreatedAt: ci.CreatedAt,
	}, nil
}

func (s *service) DoorActivity(
	ctx context.Context,
	p authz.Principal,
	sessionID uuid.UUID,
) (DoorActivity, error) {
	ev, err := s.authorizedDoorEvent(ctx, p, sessionID)
	if err != nil {
		return DoorActivity{}, err
	}
	if store, ok := s.tickets.(DoorStore); ok {
		return store.DoorSessionActivity(ctx, sessionID, 20)
	}
	listed, err := s.tickets.ListByEvent(ctx, ev.ID)
	if err != nil {
		return DoorActivity{}, err
	}
	type activityItem struct {
		ticket  Ticket
		checkIn CheckIn
	}
	pending := make([]activityItem, 0)
	for _, t := range listed {
		for _, ci := range t.CheckIns {
			if ci.SessionID != sessionID {
				continue
			}
			pending = append(pending, activityItem{ticket: t, checkIn: ci})
		}
	}
	sort.Slice(pending, func(i, j int) bool {
		return pending[i].checkIn.CreatedAt.After(pending[j].checkIn.CreatedAt)
	})
	total := len(pending)
	if len(pending) > 20 {
		pending = pending[:20]
	}
	items := make([]DoorCheckIn, 0, len(pending))
	for _, item := range pending {
		name, _, _ := s.localDoorTicketIdentity(ctx, item.ticket)
		ci := item.checkIn
		items = append(items, DoorCheckIn{
			ID: ci.ID, TicketID: ci.TicketID, SessionID: ci.SessionID, EventDayID: ci.EventDayID,
			PersonName: name, CreatedAt: ci.CreatedAt,
		})
	}
	return DoorActivity{Total: total, Items: items}, nil
}

func doorResource(ev event.Event) authz.Resource {
	ids := make([]string, 0, len(ev.DoorStaffIDs))
	for _, id := range ev.DoorStaffIDs {
		ids = append(ids, id.String())
	}
	return authz.Resource{
		Type: authz.TypeTicket, OwnerTeam: ev.OwnerTeam, DoorStaffIDs: ids,
	}
}

func principalInOwnerTeam(p authz.Principal, ownerTeam string) bool {
	if ownerTeam == "" {
		return false
	}
	marker := "/" + ownerTeam
	for _, group := range p.Groups {
		if strings.HasSuffix(group, marker) || strings.Contains(group, marker+"/") {
			return true
		}
	}
	return false
}

func (s *service) canUseDoor(ctx context.Context, p authz.Principal, ev event.Event) bool {
	resource := doorResource(ev)
	if s.authz.Allow(p, resource, authz.Validate) {
		return true
	}
	if !principalInOwnerTeam(p, ev.OwnerTeam) {
		return false
	}
	resource.TeamDoorScan = s.teamDoorScan(ctx, ev.OwnerTeam)
	return s.authz.Allow(p, resource, authz.Validate)
}

func (s *service) teamDoorScan(ctx context.Context, ownerTeam string) bool {
	if s.teams == nil || ownerTeam == "" {
		return false
	}
	g, err := s.teams.GetGroup(ctx, ownerTeam)
	if err != nil || g.Attributes == nil {
		return false
	}
	return strings.EqualFold(g.Attributes["team_door_scan"], "true")
}
