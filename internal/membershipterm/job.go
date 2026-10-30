package membershipterm

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
)

const (
	// CheckInterval is how often the schedule looks whether the daily run
	// is due and reads the gauges.
	CheckInterval = time.Hour
	// scheduleEvery is the schedule's spacing: a run is due 23 hours after
	// the last successful one started, so an hourly check keeps runs at
	// most a day apart (the retention sweep's rule). A failed run is tried
	// again at the next check.
	scheduleEvery = 23 * time.Hour
	// weeklyEvery spaces the weekly summary; an hour's slack keeps the
	// daily run's drift from pushing it to the next day.
	weeklyEvery = 7*24*time.Hour - time.Hour
	// runTimeout bounds one scheduled run.
	runTimeout = 15 * time.Minute
)

var (
	// ErrLocked: another run holds the lock.
	ErrLocked = errors.New("management terms: another run holds the lock")
	// ErrNotDue: a scheduled run found, holding the lock, that another
	// replica's run made it not due.
	ErrNotDue = errors.New("management terms: not due")
)

// RunOptions say how to run.
type RunOptions struct {
	// Mode is dry-run (count, write and send nothing) or apply.
	Mode    Mode
	Trigger Trigger
	// OnlyIfDue makes the run check, holding the lock, that it is still
	// due (the schedule).
	OnlyIfDue bool
}

// Due reports whether the scheduled run of mode is due: no scheduled run
// of it ended ok in the last 23 hours.
func (s *Service) Due(ctx context.Context, mode Mode) (bool, error) {
	last, err := s.config.Store.LastSuccess(ctx, mode, TriggerSchedule)
	if err != nil {
		return false, err
	}
	return last == nil || !s.now().Before(last.Add(scheduleEvery)), nil
}

// membership is a person directly in a management Group, as Keycloak says.
type membership struct {
	userID  uuid.UUID
	groupID uuid.UUID
	path    string
	scope   Scope
}

// Run runs the daily job once: it finds the management memberships in
// Keycloak and gives those without a term one (the active season's end),
// ends the terms whose membership is gone, dates the undated terms when an
// active season has an end, warns of the terms 14 days and 1 day from
// their end, marks those past their end expired (recorded, and listed to
// ADMIN) and, once a week, tells ADMIN how many expired terms are still in
// their Groups. Nobody is taken out of a Group. A dry run counts all this
// and writes nothing but its own record.
//
// A step that fails does not stop the others; the run then ends failed and
// is tried again at the next check. ErrLocked when another run holds the
// lock.
func (s *Service) Run(ctx context.Context, opts RunOptions) (Run, error) {
	run := Run{ID: uuid.New(), Mode: opts.Mode, Trigger: opts.Trigger}
	if opts.Mode != ModeDryRun && opts.Mode != ModeApply {
		return run, fmt.Errorf("management terms: mode %q cannot run", opts.Mode)
	}
	release, ok, err := s.config.Store.Lock(ctx)
	if err != nil {
		return run, err
	}
	if !ok {
		return run, ErrLocked
	}
	defer release()
	if opts.OnlyIfDue {
		if due, err := s.Due(ctx, opts.Mode); err != nil || !due {
			if err == nil {
				err = ErrNotDue
			}
			return run, err
		}
	}
	run.StartedAt = s.now()
	if err := s.config.Store.StartRun(ctx, run); err != nil {
		return run, err
	}
	apply := opts.Mode == ModeApply
	var failures []string
	fail := func(step string, err error) {
		failures = append(failures, step)
		s.logf("management terms: %s failed (%s)", step, errorCode(err))
	}

	def, found := s.defaultEnd(ctx)
	run.SeasonFound = found
	if !found {
		s.logf("management terms: WARN no single active season with an end ahead; terms found without one stay undated")
	}
	if err := s.reconcile(ctx, &run, apply, def, found); err != nil {
		fail("reconcile", err)
	}
	terms, err := s.config.Store.Open(ctx)
	if err != nil {
		fail("read", err)
	} else {
		if !apply {
			// What a dry run's reconcile would have dated.
			terms = s.dryDated(terms, def, found)
		}
		rosters := map[string][]uuid.UUID{}
		if err := s.warn(ctx, &run, apply, terms, rosters); err != nil {
			fail("warn", err)
		}
		if err := s.expire(ctx, &run, apply, terms, rosters); err != nil {
			fail("expire", err)
		}
		if err := s.weekly(ctx, &run, apply, rosters); err != nil {
			fail("weekly", err)
		}
	}
	run.FinishedAt = s.now()
	run.Status = "ok"
	if len(failures) > 0 {
		run.Status, run.ErrorCode = "failed", failures[0]+"_failed"
		s.count(func() { s.failures++ })
	}
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	ferr := s.config.Store.FinishRun(finishCtx, run)
	s.logf("management_terms_run id=%s mode=%s trigger=%s status=%s season=%t backfilled=%d dated=%d ended=%d warned_14d=%d warned_1d=%d expired=%d mails=%d weekly=%s",
		run.ID, run.Mode, run.Trigger, run.Status, run.SeasonFound, run.Backfilled, run.Dated, run.Ended, run.Warned14d, run.Warned1d, run.Expired, run.Mails, run.Weekly)
	if len(failures) > 0 {
		return run, errors.Join(fmt.Errorf("management terms: %v failed", failures), ferr)
	}
	return run, ferr
}

// groupListerComplete is a Directory that says when its Group list may miss
// a branch (identity.Keycloak).
type groupListerComplete interface {
	ListGroupsComplete(ctx context.Context) ([]identity.Group, error)
}

// reconcile matches the terms with Keycloak's management memberships. A
// Group list that may miss a branch (identity.ErrIncompleteGroups) would
// end the terms of every Group in it: the step then fails having added and
// ended nothing, and the next run reads again.
func (s *Service) reconcile(ctx context.Context, run *Run, apply bool, def seasonEnd, found bool) error {
	var groups []identity.Group
	var err error
	if complete, ok := s.config.Directory.(groupListerComplete); ok {
		groups, err = complete.ListGroupsComplete(ctx)
	} else {
		groups, err = s.config.Directory.ListGroups(ctx)
	}
	if err != nil {
		return err
	}
	open, err := s.config.Store.Open(ctx)
	if err != nil {
		return err
	}
	now := s.now()
	present := map[[2]uuid.UUID]bool{}
	listed := map[uuid.UUID]bool{}
	paths := map[uuid.UUID]string{}
	read := map[uuid.UUID]bool{}
	var found2 []membership
	var unread error
	for _, g := range groups {
		scope, ok := s.config.Rules.ScopeOf(g.Path)
		id, err := uuid.Parse(g.ID)
		if !ok || err != nil {
			continue
		}
		listed[id] = true
		paths[id] = g.Path
		members, err := s.config.Directory.Members(ctx, g.ID)
		if err != nil {
			// Its terms are neither added nor ended this run.
			unread = err
			continue
		}
		read[id] = true
		for _, m := range members {
			present[[2]uuid.UUID{m.ID, id}] = true
			found2 = append(found2, membership{userID: m.ID, groupID: id, path: g.Path, scope: scope})
		}
	}
	held := map[[2]uuid.UUID]bool{}
	var gone []uuid.UUID
	undated := 0
	for _, t := range open {
		key := [2]uuid.UUID{t.UserID, t.GroupID}
		held[key] = true
		switch {
		case read[t.GroupID] && !present[key], !listed[t.GroupID]:
			// Taken out (in Keycloak's console, or by a write whose term
			// was not ended), or the Group is gone or no longer one with
			// terms.
			gone = append(gone, t.ID)
		case t.EndsAt == nil:
			undated++
		}
	}
	var missing []Term
	for _, m := range found2 {
		key := [2]uuid.UUID{m.userID, m.groupID}
		if held[key] {
			continue
		}
		held[key] = true
		t := Term{ID: uuid.New(), UserID: m.userID, GroupID: m.groupID, GroupPath: m.path, Scope: m.scope, Source: SourceBackfill, StartedAt: now}
		if found {
			t.EndsAt, t.SeasonID = &def.endsAt, &def.seasonID
		}
		missing = append(missing, t)
	}
	if !apply {
		run.Backfilled, run.Ended = len(missing), len(gone)
		if found {
			run.Dated = undated
		}
		return unread
	}
	if run.Ended, err = s.config.Store.EndIDs(ctx, gone, now); err != nil {
		return err
	}
	// A Group renamed since keeps its terms, under its new path.
	repathed := map[uuid.UUID]bool{}
	for _, t := range open {
		if path, ok := paths[t.GroupID]; ok && path != t.GroupPath && !repathed[t.GroupID] {
			repathed[t.GroupID] = true
			if _, err := s.config.Store.Repath(ctx, t.GroupID, path, now); err != nil {
				return err
			}
		}
	}
	if run.Backfilled, err = s.config.Store.Insert(ctx, missing); err != nil {
		return err
	}
	if found {
		if run.Dated, err = s.config.Store.Date(ctx, def.endsAt, def.seasonID, now); err != nil {
			return err
		}
	}
	return unread
}

// dryDated are the open terms as a dry run's reconcile would have left
// them dated: what the warnings and expiries of a dry run then count.
func (s *Service) dryDated(terms []Term, def seasonEnd, found bool) []Term {
	if !found {
		return terms
	}
	out := slices.Clone(terms)
	for i := range out {
		if out[i].EndsAt == nil {
			out[i].EndsAt = &def.endsAt
		}
	}
	return out
}

// roster is the people in the trees of paths, each once, read once per run.
func (s *Service) roster(ctx context.Context, rosters map[string][]uuid.UUID, paths []string) ([]uuid.UUID, error) {
	var out []uuid.UUID
	for _, path := range paths {
		ids, ok := rosters[path]
		if !ok {
			var err error
			if ids, err = s.config.Rosters.MemberIDs(ctx, path); err != nil {
				return nil, err
			}
			rosters[path] = ids
		}
		for _, id := range ids {
			if !slices.Contains(out, id) {
				out = append(out, id)
			}
		}
	}
	return out, nil
}

// warn queues the 14-day and the 1-day warning of each term that reached
// it: to the holder and to those over them (Rules.overseers). A term warned
// once is not warned again; one found within a day of its end gets only
// the 1-day warning.
func (s *Service) warn(ctx context.Context, run *Run, apply bool, terms []Term, rosters map[string][]uuid.UUID) error {
	now := s.now()
	var failed error
	for _, t := range terms {
		if t.EndsAt == nil || t.ExpiredAt != nil || !now.Before(*t.EndsAt) {
			continue
		}
		var notice Notice
		switch {
		case !now.Before(t.EndsAt.Add(-WarnLate)) && t.Warned1d == nil:
			notice = NoticeWarning1d
		case !now.Before(t.EndsAt.Add(-WarnEarly)) && t.Warned14d == nil && t.Warned1d == nil:
			notice = NoticeWarning14d
		default:
			continue
		}
		overseers, err := s.roster(ctx, rosters, s.config.Rules.overseers(t.GroupPath))
		if err != nil {
			// Tried again at the next run.
			failed = err
			continue
		}
		recipients := append([]uuid.UUID{t.UserID}, slices.DeleteFunc(overseers, func(id uuid.UUID) bool { return id == t.UserID })...)
		mails := make([]Mail, 0, len(recipients))
		for _, r := range recipients {
			termID, subject, group := t.ID, t.UserID, t.GroupID
			mails = append(mails, Mail{
				RecipientID: r, Notice: notice, TermID: &termID, SubjectID: &subject, GroupID: &group, GroupPath: t.GroupPath,
				EndsAt: t.EndsAt, OccurredAt: now,
			})
		}
		warned := true
		if apply {
			if warned, err = s.config.Store.Warn(ctx, t.ID, *t.EndsAt, notice, now, mails); err != nil {
				failed = err
				continue
			}
		}
		if !warned {
			continue
		}
		run.Mails += len(mails)
		if notice == NoticeWarning1d {
			run.Warned1d++
		} else {
			run.Warned14d++
		}
	}
	return failed
}

// expire marks the terms past their end expired: each is recorded
// (membership.term.expired, by the system) and the day's list goes to
// ADMIN. Nobody is taken out of a Group, and Keycloak is not written.
func (s *Service) expire(ctx context.Context, run *Run, apply bool, terms []Term, rosters map[string][]uuid.UUID) error {
	now := s.now()
	var due []Term
	for _, t := range terms {
		if t.EndsAt != nil && t.ExpiredAt == nil && !now.Before(*t.EndsAt) {
			due = append(due, t)
		}
	}
	if len(due) == 0 {
		return nil
	}
	admins, err := s.roster(ctx, rosters, s.config.Rules.adminRoots())
	if err != nil {
		// Marked at the next run, with its list.
		return err
	}
	if !apply {
		run.Expired = len(due)
		run.Mails += len(admins)
		return nil
	}
	records := map[uuid.UUID]uuid.UUID{}
	ids := make([]uuid.UUID, 0, len(due))
	var failed error
	for _, t := range due {
		record, err := s.config.Audit.Begin(ctx, teamaudit.Record{
			Actor:  teamaudit.Actor{Basis: teamaudit.BasisSystem, Client: teamaudit.SystemTermExpiry},
			Action: teamaudit.ActionTermExpired, GroupID: t.GroupID.String(), GroupPath: t.GroupPath, TargetUserID: t.UserID,
			After: &teamaudit.State{EndsAt: timeText(t.EndsAt)},
		})
		if err != nil {
			// No record, no mark: tried again at the next run.
			failed = err
			continue
		}
		records[t.ID] = record
		ids = append(ids, t.ID)
	}
	marked, err := s.config.Store.Expire(ctx, ids, now, admins)
	for id, record := range records {
		outcome, code := teamaudit.OutcomeOK, ""
		switch {
		case err != nil:
			outcome, code = teamaudit.OutcomeFailed, "store_error"
		case !slices.Contains(marked, id):
			// Extended or ended meanwhile.
			outcome, code = teamaudit.OutcomeFailed, "unchanged"
		}
		if ferr := s.config.Audit.Finish(ctx, record, outcome, code, nil); ferr != nil {
			s.logf("management terms: audit record %s stays pending: %s", record, errorCode(ferr))
		}
	}
	if err != nil {
		return err
	}
	run.Expired = len(marked)
	if len(marked) > 0 {
		run.Mails += len(admins)
	}
	return failed
}

// weekly tells ADMIN, once a week, how many expired terms are still in
// their Groups, by Group. Nothing goes when there is none.
func (s *Service) weekly(ctx context.Context, run *Run, apply bool, rosters map[string][]uuid.UUID) error {
	if !s.config.Weekly {
		run.Weekly = WeeklyOff
		return nil
	}
	now := s.now()
	last, err := s.config.Store.LastWeekly(ctx)
	if err != nil {
		return err
	}
	if last != nil && now.Before(last.Add(weeklyEvery)) {
		run.Weekly = WeeklyNotDue
		return nil
	}
	terms, err := s.config.Store.Open(ctx)
	if err != nil {
		return err
	}
	var summary []GroupCount
	total := 0
	for _, t := range terms {
		if t.Status(now) != StatusExpired {
			continue
		}
		total++
		i := slices.IndexFunc(summary, func(c GroupCount) bool { return c.GroupID == t.GroupID.String() })
		if i < 0 {
			summary = append(summary, GroupCount{GroupID: t.GroupID.String(), Path: t.GroupPath})
			i = len(summary) - 1
		}
		summary[i].Count++
	}
	if total == 0 {
		run.Weekly = WeeklyNone
		return nil
	}
	admins, err := s.roster(ctx, rosters, s.config.Rules.adminRoots())
	if err != nil {
		return err
	}
	mails := make([]Mail, 0, len(admins))
	for _, r := range admins {
		mails = append(mails, Mail{RecipientID: r, Notice: NoticeWeeklySummary, Summary: summary, OccurredAt: now})
	}
	if apply && len(mails) > 0 {
		if err := s.config.Store.Enqueue(ctx, mails); err != nil {
			return err
		}
	}
	run.Mails += len(mails)
	run.Weekly = WeeklySent
	return nil
}

// Maintain runs the job in the configured mode whenever it is due, looking
// at once and then every interval, and refreshes the gauges after each
// look. A run another replica holds is not an error. The returned channel
// closes after ctx is cancelled. With the mode off it starts nothing.
func (s *Service) Maintain(ctx context.Context, interval time.Duration) <-chan struct{} {
	done := make(chan struct{})
	mode := s.config.Mode
	if mode == ModeOff {
		close(done)
		return done
	}
	go func() {
		defer close(done)
		timer := time.NewTimer(0)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				due, err := s.Due(ctx, mode)
				if err != nil && ctx.Err() == nil {
					s.logf("management terms: schedule: %s", errorCode(err))
				}
				if err == nil && due {
					runCtx, cancel := context.WithTimeout(ctx, runTimeout)
					_, err := s.Run(runCtx, RunOptions{Mode: mode, Trigger: TriggerSchedule, OnlyIfDue: true})
					cancel()
					if err != nil && !errors.Is(err, ErrLocked) && !errors.Is(err, ErrNotDue) && ctx.Err() == nil {
						s.logf("management terms: run: %v", err)
					}
				}
				s.Refresh(ctx)
				timer.Reset(interval)
			}
		}
	}()
	return done
}
