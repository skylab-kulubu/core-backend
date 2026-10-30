package membershipterm

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/season"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
	"github.com/skylab-kulubu/core-backend/internal/teammail"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// Directory reads the Groups and their direct members (identity.Directory).
type Directory interface {
	ListGroups(ctx context.Context) ([]identity.Group, error)
	GetGroup(ctx context.Context, idOrPath string) (identity.Group, error)
	Members(ctx context.Context, groupID string) ([]identity.Person, error)
}

// Seasons reads the seasons (season.Store).
type Seasons interface {
	List(ctx context.Context, activeOnly bool) ([]season.Season, error)
}

// Accounts reads core's rows of people, for the names a list shows
// (user.Store).
type Accounts interface {
	Accounts(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]user.Account, error)
}

// Mailer sends the term mails (mail.SkyMail).
type Mailer interface {
	ManagementTermConfigured() bool
	ManagementTerm(ctx context.Context, recipient, fullName string, vars map[string]string) error
}

// Memberships changes and decides Group memberships as the member routes
// do (identity.Service): its writes are authorized, recorded and mailed
// there.
type Memberships interface {
	AddMember(ctx context.Context, p authz.Principal, groupRef string, userID uuid.UUID) error
	RemoveMember(ctx context.Context, p authz.Principal, groupRef string, userID uuid.UUID) error
	MembershipWriteDecision(ctx context.Context, p authz.Principal, groupRef string) (identity.MembershipDecision, error)
}

// Config is what a Service takes.
type Config struct {
	Mode Mode
	// Weekly turns the weekly summary to ADMIN on.
	Weekly    bool
	Rules     Rules
	Store     Store
	Directory Directory
	Seasons   Seasons
	// Rosters reads who sits in a Group's tree (teammail.DirectoryRosters).
	Rosters teammail.Rosters
	// People reads a mail's recipient and names (teammail.DirectoryPeople).
	People   teammail.People
	Accounts Accounts
	// Mail sends the mails; nil (or without a template) drops each at its
	// turn.
	Mail  Mailer
	Audit teamaudit.Writer
	Authz authz.Authorizer
	// Memberships serve the routes; nil serves none.
	Memberships Memberships
	Now         func() time.Time
	Logf        func(string, ...any)
}

// Service keeps the terms: it hears of the membership routes' changes
// (identity.MembershipNotifier), runs the daily job (Run), sends the mails
// (Pass) and serves the routes.
type Service struct {
	config Config
	now    func() time.Time
	logf   func(string, ...any)

	mu       sync.Mutex
	counts   Counts
	counted  bool
	hook     hookCounts
	outcomes map[string]int64
	backlog  int
	oldest   time.Duration
	failures int64
}

type hookCounts struct {
	started, ended, undated, errors, precheck int64
}

// NewService checks config.
func NewService(config Config) (*Service, error) {
	switch {
	case config.Mode != ModeOff && config.Mode != ModeDryRun && config.Mode != ModeApply:
		return nil, fmt.Errorf("membershipterm: mode %q", config.Mode)
	case config.Store == nil:
		return nil, errors.New("membershipterm: no store")
	case config.Directory == nil || config.Seasons == nil || config.Rosters == nil:
		return nil, errors.New("membershipterm: no directory, seasons or rosters")
	case config.Audit == nil:
		return nil, errors.New("membershipterm: no audit writer")
	}
	if config.Rules.PrivilegedRoots == nil {
		config.Rules = DefaultRules()
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	logf := config.Logf
	if logf == nil {
		logf = log.Printf
	}
	return &Service{config: config, now: func() time.Time { return now().UTC() }, logf: logf, outcomes: map[string]int64{}}, nil
}

// UseMemberships serves the routes with m: the identity service, which
// itself tells s of its membership writes, so it is built after s.
func (s *Service) UseMemberships(m Memberships) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.config.Memberships = m
}

// Mode is the configured mode.
func (s *Service) Mode() Mode { return s.config.Mode }

func (s *Service) applies() bool { return s != nil && s.config.Mode == ModeApply }

func (s *Service) count(update func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	update()
}

// hookTimeout bounds what a membership route waits for the term's write.
const hookTimeout = 3 * time.Second

// Notifies reports whether a membership of g has a term: a management
// Group, in apply mode.
func (s *Service) Notifies(g identity.Group) bool {
	if !s.applies() {
		return false
	}
	_, ok := s.config.Rules.ScopeOf(g.Path)
	return ok
}

// MembershipChanged starts a term for a person added through core (ending
// any open one of the same Group: an add is a new term) and ends the open
// term of one removed. It never fails the request: a write that fails is
// logged and counted, and the daily job finds the membership.
func (s *Service) MembershipChanged(ctx context.Context, change identity.MembershipChange) {
	scope, ok := s.config.Rules.ScopeOf(change.Group.Path)
	groupID, err := uuid.Parse(change.Group.ID)
	if !s.applies() || !ok || err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), hookTimeout)
	defer cancel()
	now := s.now()
	if !change.Added {
		if _, err := s.config.Store.End(ctx, change.UserID, groupID, now); err != nil {
			s.count(func() { s.hook.errors++ })
			s.logf("management terms: a removal's term could not be ended (%s); the daily job ends it", errorCode(err))
			return
		}
		s.count(func() { s.hook.ended++ })
		return
	}
	t := Term{ID: uuid.New(), UserID: change.UserID, GroupID: groupID, GroupPath: change.Group.Path, Scope: scope, Source: SourceRoute, StartedAt: now}
	if actor, err := uuid.Parse(change.ActorID); err == nil {
		t.GrantedBy = &actor
	}
	if def, ok := s.defaultEnd(ctx); ok {
		t.EndsAt, t.SeasonID = &def.endsAt, &def.seasonID
	} else {
		s.count(func() { s.hook.undated++ })
		s.logf("management terms: WARN no single active season with an end ahead; the new term has no end until one is set")
	}
	if err := s.config.Store.Restart(ctx, t); err != nil {
		s.count(func() { s.hook.errors++ })
		s.logf("management terms: an add's term could not be written (%s); the daily job gives it one", errorCode(err))
		return
	}
	s.count(func() { s.hook.started++ })
}

// PrecheckFailed is told that the reads that tell an add from a repeat
// failed: no term was started; the daily job finds the membership.
func (s *Service) PrecheckFailed() {
	s.count(func() { s.hook.precheck++ })
}

var _ identity.MembershipNotifier = (*Service)(nil)

// seasonEnd is the default end of a new term: the active season's last
// day, end of day in Istanbul.
type seasonEnd struct {
	endsAt   time.Time
	seasonID uuid.UUID
}

// defaultEnd reads the one active season with an end still ahead. None,
// more than one, or one whose last day has passed (the next season not set
// active yet) gives no default: a term without an end, and a warning.
func (s *Service) defaultEnd(ctx context.Context) (seasonEnd, bool) {
	seasons, err := s.config.Seasons.List(ctx, true)
	if err != nil {
		return seasonEnd{}, false
	}
	var found []season.Season
	for _, item := range seasons {
		if item.Active && item.ArchivedAt == nil && item.EndDate != nil {
			found = append(found, item)
		}
	}
	if len(found) != 1 {
		return seasonEnd{}, false
	}
	end := EndOfDay(*found[0].EndDate)
	if !end.After(s.now()) {
		return seasonEnd{}, false
	}
	return seasonEnd{endsAt: end, seasonID: found[0].ID}, true
}

// latestEnd is the latest end an extension may give at now: the end of the
// next season (the earliest starting after the active season's end, or,
// when no active season has an end ahead, the earliest not over yet: one
// entered but not set active counts even once begun), or a year on
// (MaxExtension) when no next season is defined (Yusuf, 2026-10-19).
func (s *Service) latestEnd(ctx context.Context, now time.Time) (time.Time, error) {
	seasons, err := s.config.Seasons.List(ctx, false)
	if err != nil {
		return time.Time{}, err
	}
	from, currentID, between := now, uuid.Nil, true
	if current, ok := s.defaultEnd(ctx); ok {
		from, currentID, between = current.endsAt, current.seasonID, false
	}
	var next *season.Season
	for i := range seasons {
		item := seasons[i]
		if item.ArchivedAt != nil || item.StartDate == nil || item.EndDate == nil || item.ID == currentID {
			continue
		}
		// A day's slack: a season stored at midnight UTC starts on the
		// Istanbul day the active one's end closes.
		ahead := !item.StartDate.Before(from.Add(-24*time.Hour)) || (between && EndOfDay(*item.EndDate).After(now))
		if ahead && (next == nil || item.StartDate.Before(*next.StartDate)) {
			next = &item
		}
	}
	if next == nil {
		return EndOfDay(now.Add(MaxExtension)), nil
	}
	return EndOfDay(*next.EndDate), nil
}

// seasonEnding is the season whose last day endsAt is, if any.
func (s *Service) seasonEnding(ctx context.Context, endsAt time.Time) *uuid.UUID {
	seasons, err := s.config.Seasons.List(ctx, false)
	if err != nil {
		return nil
	}
	for _, item := range seasons {
		if item.ArchivedAt == nil && item.EndDate != nil && EndOfDay(*item.EndDate).Equal(endsAt) {
			id := item.ID
			return &id
		}
	}
	return nil
}

// Item is a term as the routes answer it.
type Item struct {
	ID        uuid.UUID  `json:"id"`
	UserID    uuid.UUID  `json:"userId"`
	FirstName string     `json:"firstName"`
	LastName  string     `json:"lastName"`
	GroupID   uuid.UUID  `json:"groupId"`
	GroupPath string     `json:"groupPath"`
	Role      string     `json:"role"`
	Status    Status     `json:"status"`
	EndsAt    *time.Time `json:"endsAt"`
	SeasonID  *uuid.UUID `json:"seasonId"`
	Source    Source     `json:"source"`
	StartedAt time.Time  `json:"startedAt"`
	GrantedBy *uuid.UUID `json:"grantedBy"`
	Warned14d *time.Time `json:"warned14dAt"`
	Warned1d  *time.Time `json:"warned1dAt"`
	ExpiredAt *time.Time `json:"expiredAt"`
}

// Filter narrows the list: a season, a role (Rules.Role), a status.
type Filter struct {
	SeasonID *uuid.UUID
	Role     string
	Status   Status
}

// ValidRole reports whether role is a list filter's role.
func ValidRole(role string) bool {
	switch role {
	case RoleBoard, RoleAudit, RolePrivileged, RoleTeamLeaders:
		return true
	}
	return false
}

// ValidStatus reports whether status is a list filter's status.
func ValidStatus(status Status) bool {
	return status == StatusActive || status == StatusExpiring || status == StatusExpired
}

// List is the open terms the caller may see, by end then path: every one
// for a reader of Groups (Privileged, DK), and for anyone else those of
// the Groups whose membership they may change.
func (s *Service) List(ctx context.Context, p authz.Principal, f Filter) ([]Item, error) {
	if !s.applies() || s.config.Memberships == nil {
		return nil, ErrOff
	}
	terms, err := s.config.Store.Open(ctx)
	if err != nil {
		return nil, err
	}
	now := s.now()
	all := s.config.Authz != nil && s.config.Authz.Allow(p, authz.Resource{Type: authz.TypeGroup}, authz.Read)
	allowed := map[uuid.UUID]bool{}
	var shown []Term
	for _, t := range terms {
		if f.SeasonID != nil && (t.SeasonID == nil || *t.SeasonID != *f.SeasonID) {
			continue
		}
		if f.Role != "" && s.config.Rules.Role(t.GroupPath) != f.Role {
			continue
		}
		if f.Status != "" && t.Status(now) != f.Status {
			continue
		}
		if !all {
			ok, seen := allowed[t.GroupID]
			if !seen {
				_, err := s.config.Memberships.MembershipWriteDecision(ctx, p, t.GroupID.String())
				var refused *identity.DelegationError
				switch {
				case err == nil:
					ok = true
				case errors.Is(err, identity.ErrForbidden), errors.As(err, &refused), errors.Is(err, identity.ErrNotFound):
					ok = false
				default:
					return nil, err
				}
				allowed[t.GroupID] = ok
			}
			if !ok {
				continue
			}
		}
		shown = append(shown, t)
	}
	return s.items(ctx, shown, now)
}

// items are the terms with the names core shows of their holders: none for
// someone erased, being erased or gone.
func (s *Service) items(ctx context.Context, terms []Term, now time.Time) ([]Item, error) {
	ids := make([]uuid.UUID, 0, len(terms))
	for _, t := range terms {
		ids = append(ids, t.UserID)
	}
	accounts := map[uuid.UUID]user.Account{}
	if s.config.Accounts != nil && len(ids) > 0 {
		var err error
		if accounts, err = s.config.Accounts.Accounts(ctx, ids); err != nil {
			return nil, err
		}
	}
	out := make([]Item, 0, len(terms))
	for _, t := range terms {
		item := Item{
			ID: t.ID, UserID: t.UserID, GroupID: t.GroupID, GroupPath: t.GroupPath, Role: s.config.Rules.Role(t.GroupPath),
			Status: t.Status(now), EndsAt: t.EndsAt, SeasonID: t.SeasonID, Source: t.Source, StartedAt: t.StartedAt,
			GrantedBy: t.GrantedBy, Warned14d: t.Warned14d, Warned1d: t.Warned1d, ExpiredAt: t.ExpiredAt,
		}
		if a, ok := accounts[t.UserID]; ok && a.Stored && !a.Blocked && a.User.AccountState == user.AccountActive {
			item.FirstName, item.LastName = a.User.FirstName, a.User.LastName
		}
		out = append(out, item)
	}
	return out, nil
}

// openTerm is the term id, open, for a route; ErrNotFound otherwise.
func (s *Service) openTerm(ctx context.Context, id uuid.UUID) (Term, error) {
	if !s.applies() || s.config.Memberships == nil {
		return Term{}, ErrOff
	}
	t, err := s.config.Store.Get(ctx, id)
	if err != nil {
		return Term{}, err
	}
	if t.EndedAt != nil {
		return Term{}, ErrNotFound
	}
	return t, nil
}

// checkEnd refuses an end that is not after now, or past latestEnd.
func (s *Service) checkEnd(ctx context.Context, endsAt, now time.Time) error {
	if !endsAt.After(now) {
		return ErrInvalid
	}
	limit, err := s.latestEnd(ctx, now)
	if err != nil {
		return err
	}
	if endsAt.After(limit) {
		return &RefusalError{Status: http.StatusUnprocessableEntity, Code: CodeTermTooLong}
	}
	return nil
}

// actor is the audit actor of ctx with the decision's basis.
func actor(ctx context.Context, d identity.MembershipDecision) teamaudit.Actor {
	a, _ := teamaudit.ActorFrom(ctx)
	a.Basis, a.ViaGroupID = d.Basis, d.ViaGroupID
	return a
}

func self(p authz.Principal, id uuid.UUID) bool {
	caller, err := uuid.Parse(p.ID)
	return err == nil && caller == id
}

func timeText(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// Extend gives the term id the end endsAt (an Istanbul day's end): anyone
// who may change the Group's membership, but not its holder (nobody
// extends their own term). The expiry mark and the warnings go; the new
// end is warned of again. It is recorded (membership.term.extend).
func (s *Service) Extend(ctx context.Context, p authz.Principal, id uuid.UUID, endsAt time.Time) (Item, error) {
	t, err := s.openTerm(ctx, id)
	if err != nil {
		return Item{}, err
	}
	if self(p, t.UserID) {
		return Item{}, &RefusalError{Status: http.StatusForbidden, Code: CodeSelfExtension}
	}
	d, err := s.config.Memberships.MembershipWriteDecision(ctx, p, t.GroupID.String())
	if err != nil {
		return Item{}, err
	}
	now := s.now()
	if err := s.checkEnd(ctx, endsAt, now); err != nil {
		return Item{}, err
	}
	record, err := s.config.Audit.Begin(ctx, teamaudit.Record{
		Actor: actor(ctx, d), Action: teamaudit.ActionTermExtend, GroupID: t.GroupID.String(), GroupPath: d.Group.Path,
		TargetUserID: t.UserID, Before: &teamaudit.State{EndsAt: timeText(t.EndsAt)}, After: &teamaudit.State{EndsAt: timeText(&endsAt)},
	})
	if err != nil {
		return Item{}, err
	}
	// The season is the one whose last day the new end is; an end on no
	// season's last day keeps the term's season.
	seasonID := s.seasonEnding(ctx, endsAt)
	if seasonID == nil {
		seasonID = t.SeasonID
	}
	err = s.config.Store.Extend(ctx, t.ID, endsAt, seasonID, now)
	s.finish(ctx, record, err)
	if err != nil {
		return Item{}, err
	}
	return s.item(ctx, t.ID)
}

// finish writes the outcome of an audit row: ok, or failed.
func (s *Service) finish(ctx context.Context, record uuid.UUID, err error) {
	outcome, code := teamaudit.OutcomeOK, ""
	if err != nil {
		outcome, code = teamaudit.OutcomeFailed, auditCode(err)
	}
	if ferr := s.config.Audit.Finish(ctx, record, outcome, code, nil); ferr != nil {
		s.logf("management terms: audit record %s stays pending: %s", record, errorCode(ferr))
	}
}

func (s *Service) item(ctx context.Context, id uuid.UUID) (Item, error) {
	t, err := s.config.Store.Get(ctx, id)
	if err != nil {
		return Item{}, err
	}
	items, err := s.items(ctx, []Term{t}, s.now())
	if err != nil {
		return Item{}, err
	}
	return items[0], nil
}

// HandoverMode is how a handover treats the holder.
type HandoverMode string

const (
	// HandoverImmediate takes the holder out of the Group at once.
	HandoverImmediate HandoverMode = "immediate"
	// HandoverAtTermEnd leaves the holder in; their term is marked at its
	// own end.
	HandoverAtTermEnd HandoverMode = "at_term_end"
)

// HandoverInput is a handover's request.
type HandoverInput struct {
	UserID uuid.UUID
	Mode   HandoverMode
	// EndsAt is the successor's end; nil is the active season's.
	EndsAt *time.Time
}

// Handover adds in.UserID to the term's Group through the member route's
// way (authorized, recorded and mailed there) and gives them a term; with
// HandoverImmediate it then removes the holder the same way. Nobody hands a
// term to themselves. It is recorded (membership.term.handover) and answers
// the successor's term.
func (s *Service) Handover(ctx context.Context, p authz.Principal, id uuid.UUID, in HandoverInput) (Item, error) {
	t, err := s.openTerm(ctx, id)
	if err != nil {
		return Item{}, err
	}
	switch {
	case in.UserID == uuid.Nil || in.UserID == t.UserID, in.Mode != HandoverImmediate && in.Mode != HandoverAtTermEnd:
		return Item{}, ErrInvalid
	case self(p, in.UserID):
		return Item{}, &RefusalError{Status: http.StatusForbidden, Code: CodeSelfExtension}
	}
	d, err := s.config.Memberships.MembershipWriteDecision(ctx, p, t.GroupID.String())
	if err != nil {
		return Item{}, err
	}
	now := s.now()
	successor := Term{
		ID: uuid.New(), UserID: in.UserID, GroupID: t.GroupID, GroupPath: d.Group.Path, Scope: t.Scope,
		Source: SourceHandover, StartedAt: now,
	}
	if caller, err := uuid.Parse(p.ID); err == nil {
		successor.GrantedBy = &caller
	}
	if in.EndsAt != nil {
		if err := s.checkEnd(ctx, *in.EndsAt, now); err != nil {
			return Item{}, err
		}
		successor.EndsAt, successor.SeasonID = in.EndsAt, s.seasonEnding(ctx, *in.EndsAt)
	} else if def, ok := s.defaultEnd(ctx); ok {
		successor.EndsAt, successor.SeasonID = &def.endsAt, &def.seasonID
	}
	record, err := s.config.Audit.Begin(ctx, teamaudit.Record{
		Actor: actor(ctx, d), Action: teamaudit.ActionTermHandover, GroupID: t.GroupID.String(), GroupPath: d.Group.Path,
		TargetUserID: t.UserID, Before: &teamaudit.State{EndsAt: timeText(t.EndsAt)},
		After: &teamaudit.State{EndsAt: timeText(successor.EndsAt), SuccessorID: in.UserID.String(), Handover: string(in.Mode)},
	})
	if err != nil {
		return Item{}, err
	}
	err = s.handover(ctx, p, t, successor, in.Mode)
	s.finish(ctx, record, err)
	if err != nil {
		return Item{}, err
	}
	terms, err := s.config.Store.OpenFor(ctx, in.UserID)
	if err != nil {
		return Item{}, err
	}
	for _, open := range terms {
		if open.GroupID == t.GroupID {
			items, err := s.items(ctx, []Term{open}, s.now())
			if err != nil {
				return Item{}, err
			}
			return items[0], nil
		}
	}
	return Item{}, ErrNotFound
}

func (s *Service) handover(ctx context.Context, p authz.Principal, t, successor Term, mode HandoverMode) error {
	if err := s.config.Memberships.AddMember(ctx, p, t.GroupID.String(), successor.UserID); err != nil {
		return err
	}
	// The add's own term (MembershipChanged) may be there already; the
	// handover's end then replaces its end. A successor who was in the
	// Group keeps their term, with the handover's end when one was given.
	if _, err := s.config.Store.Insert(ctx, []Term{successor}); err != nil {
		return err
	}
	open, err := s.config.Store.OpenFor(ctx, successor.UserID)
	if err != nil {
		return err
	}
	for _, o := range open {
		if o.GroupID == t.GroupID && o.ID != successor.ID && successor.EndsAt != nil && (o.EndsAt == nil || !o.EndsAt.Equal(*successor.EndsAt)) {
			if err := s.config.Store.Extend(ctx, o.ID, *successor.EndsAt, successor.SeasonID, s.now()); err != nil {
				return err
			}
		}
	}
	if mode != HandoverImmediate {
		return nil
	}
	if err := s.config.Memberships.RemoveMember(ctx, p, t.GroupID.String(), t.UserID); err != nil {
		return err
	}
	_, err = s.config.Store.End(ctx, t.UserID, t.GroupID, s.now())
	return err
}

// Remove takes an expired term's holder out of its Group through the
// member route's way (authorized, recorded, mailed and noticed there) and
// ends the term. A term that has not expired is the member route's.
func (s *Service) Remove(ctx context.Context, p authz.Principal, id uuid.UUID) error {
	t, err := s.openTerm(ctx, id)
	if err != nil {
		return err
	}
	if t.Status(s.now()) != StatusExpired {
		return &RefusalError{Status: http.StatusConflict, Code: CodeTermNotExpired}
	}
	if err := s.config.Memberships.RemoveMember(ctx, p, t.GroupID.String(), t.UserID); err != nil {
		return err
	}
	_, err = s.config.Store.End(ctx, t.UserID, t.GroupID, s.now())
	return err
}

// ExpiredMembership is one of the caller's expired management memberships,
// for the capabilities (the panel's banner).
type ExpiredMembership struct {
	TermID    uuid.UUID  `json:"termId"`
	GroupID   uuid.UUID  `json:"groupId"`
	GroupPath string     `json:"groupPath"`
	Role      string     `json:"role"`
	EndsAt    *time.Time `json:"endsAt"`
	ExpiredAt *time.Time `json:"expiredAt"`
}

// ExpiredFor are the person's expired management memberships still open;
// empty unless the mode is apply.
func (s *Service) ExpiredFor(ctx context.Context, userID uuid.UUID) ([]ExpiredMembership, error) {
	out := []ExpiredMembership{}
	if !s.applies() {
		return out, nil
	}
	terms, err := s.config.Store.OpenFor(ctx, userID)
	if err != nil {
		return out, err
	}
	now := s.now()
	for _, t := range terms {
		if t.Status(now) == StatusExpired {
			out = append(out, ExpiredMembership{
				TermID: t.ID, GroupID: t.GroupID, GroupPath: t.GroupPath, Role: s.config.Rules.Role(t.GroupPath),
				EndsAt: t.EndsAt, ExpiredAt: t.ExpiredAt,
			})
		}
	}
	return out, nil
}

// auditCode is the failed row's code of err (teamaudit.ValidErrorCode).
func auditCode(err error) string {
	var refused *identity.DelegationError
	switch {
	case errors.Is(err, identity.ErrNotFound), errors.Is(err, ErrNotFound):
		return "not_found"
	case errors.Is(err, identity.ErrForbidden):
		return "forbidden"
	case errors.Is(err, identity.ErrInvalid), errors.Is(err, ErrInvalid):
		return "invalid"
	case errors.As(err, &refused) && teamaudit.ValidErrorCode(refused.Code):
		return refused.Code
	case errors.Is(err, teamaudit.ErrUnavailable):
		return "audit_unavailable"
	}
	return "error"
}

// errorCode names an error for a log line without its text, which can
// quote a value.
func errorCode(err error) string {
	switch {
	case err == nil:
		return "-"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, identity.ErrNotFound):
		return "not_found"
	}
	return "error"
}
