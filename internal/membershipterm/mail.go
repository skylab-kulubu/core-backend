package membershipterm

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/mail"
	"github.com/skylab-kulubu/core-backend/internal/teammail"
)

const (
	// giveUpAfter, lease, the backoff and the send's bound are the
	// membership mail's (internal/teammail).
	giveUpAfter  = teammail.GiveUpAfter
	lease        = 2 * time.Minute
	retryFirst   = teammail.RetryFirst
	retryMax     = 30 * time.Minute
	sendTimeout  = 45 * time.Second
	writeTimeout = 5 * time.Second
	maxSends     = 200
	// pollInterval is how often the worker looks at the queue.
	pollInterval = time.Minute
	dateLayout   = "02.01.2006"
)

// outcomeNames are the mail outcomes /v1/metrics always lists.
var outcomeNames = []string{
	"sent", "failed", "rejected", "expired",
	"skipped_" + string(teammail.SkipInactive), "skipped_" + string(teammail.SkipNoEmail), "skipped_" + string(teammail.SkipKindOff),
	skippedErased,
}

// skippedErased: an expired list whose every term was erased with its
// holder since it was queued; it would list nobody, so it does not go.
const skippedErased = "skipped_erased"

func backoff(failures int) time.Duration {
	wait := retryFirst
	for i := 1; i < failures && wait < retryMax; i++ {
		wait *= 2
	}
	return min(wait, retryMax)
}

// Pass drops the mails past 72 hours, then sends the due ones until none
// is due. A failed send waits for its backoff; the error is the store's.
func (s *Service) Pass(ctx context.Context) (int, error) {
	store := s.config.Store
	expired, err := store.DropBefore(ctx, s.now().Add(-giveUpAfter))
	if err != nil {
		return 0, err
	}
	s.outcome("expired", expired)
	handled := expired
	for range maxSends {
		m, err := store.Claim(ctx, s.now(), lease)
		if err != nil {
			return handled, err
		}
		if m == nil {
			break
		}
		outcome := s.deliver(ctx, *m)
		if outcome == "interrupted" {
			return handled, ctx.Err()
		}
		s.outcome(outcome, 1)
		handled++
		writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
		if outcome == "failed" {
			err = store.Retry(writeCtx, *m, s.now().Add(backoff(m.Attempts+1)))
		} else {
			err = store.Complete(writeCtx, *m)
		}
		cancel()
		if err != nil {
			return handled, err
		}
	}
	backlog, oldest, err := store.Backlog(ctx, s.now())
	if err != nil {
		return handled, err
	}
	s.count(func() { s.backlog, s.oldest = backlog, oldest })
	return handled, nil
}

func (s *Service) outcome(name string, n int) {
	if n > 0 {
		s.count(func() { s.outcomes[name] += int64(n) })
	}
}

// deliver sends one mail and names the outcome: sent, skipped_<reason>,
// rejected, failed (retried) or interrupted.
func (s *Service) deliver(parent context.Context, m Mail) string {
	ctx, cancel := context.WithTimeout(parent, sendTimeout)
	defer cancel()
	if s.config.Mail == nil || !s.config.Mail.ManagementTermConfigured() || s.config.People == nil {
		return "skipped_" + string(teammail.SkipKindOff)
	}
	recipient, skip, err := s.config.People.Recipient(ctx, m.RecipientID)
	if err != nil {
		if parent.Err() != nil {
			return "interrupted"
		}
		return "failed"
	}
	if skip != "" {
		return "skipped_" + string(skip)
	}
	vars, err := s.vars(ctx, m, recipient)
	if err != nil {
		if parent.Err() != nil {
			return "interrupted"
		}
		return "failed"
	}
	if m.Notice == NoticeExpiredList && vars["Count"] == "0" {
		return skippedErased
	}
	err = s.config.Mail.ManagementTerm(ctx, recipient.Email, recipient.FullName, vars)
	var refused *mail.SendError
	switch {
	case err == nil:
		return "sent"
	case errors.Is(err, mail.ErrTemplateUnconfigured):
		return "skipped_" + string(teammail.SkipKindOff)
	case errors.As(err, &refused) && refused.Permanent():
		return "rejected"
	case parent.Err() != nil:
		return "interrupted"
	}
	return "failed"
}

// The template's Notice variable of each mail.
var noticeNames = map[Notice]string{
	NoticeWarning14d:    "warning",
	NoticeWarning1d:     "warning",
	NoticeExpiredList:   "expired-list",
	NoticeWeeklySummary: "weekly-summary",
}

// vars are the template's variables of a mail to recipient. Every key is
// always sent, empty when it does not apply. No e-mail address.
func (s *Service) vars(ctx context.Context, m Mail, recipient teammail.Recipient) (map[string]string, error) {
	vars := map[string]string{
		"Notice": noticeNames[m.Notice], "DaysLeft": "", "Self": "no", "SubjectName": "", "GroupName": "", "GroupId": "",
		"Role": "", "EndsAt": "", "Count": "", "Items": "",
	}
	switch m.Notice {
	case NoticeWarning14d, NoticeWarning1d:
		vars["DaysLeft"] = "14"
		if m.Notice == NoticeWarning1d {
			vars["DaysLeft"] = "1"
		}
		if m.SubjectID != nil {
			if *m.SubjectID == m.RecipientID {
				vars["Self"] = "yes"
				vars["SubjectName"] = recipient.FullName
			} else {
				vars["SubjectName"] = s.config.People.DisplayName(ctx, *m.SubjectID)
			}
		}
		ref := m.GroupPath
		if m.GroupID != nil {
			ref = m.GroupID.String()
		}
		group := s.group(ctx, ref, m.GroupPath)
		vars["GroupName"], vars["GroupId"] = s.groupName(ctx, group), group.ID
		vars["Role"] = s.config.Rules.Role(m.GroupPath)
		if m.EndsAt != nil {
			vars["EndsAt"] = m.EndsAt.In(istanbul).Format(dateLayout)
		}
	case NoticeExpiredList:
		var lines []string
		for _, id := range m.TermIDs {
			t, err := s.config.Store.Get(ctx, id)
			if errors.Is(err, ErrNotFound) {
				// Erased with its holder since.
				continue
			}
			if err != nil {
				return nil, err
			}
			name := s.config.People.DisplayName(ctx, t.UserID)
			if name == "" {
				name = "—"
			}
			ends := ""
			if t.EndsAt != nil {
				ends = t.EndsAt.In(istanbul).Format(dateLayout)
			}
			lines = append(lines, name+" · "+s.groupName(ctx, s.group(ctx, t.GroupID.String(), t.GroupPath))+" · "+ends)
		}
		vars["Count"], vars["Items"] = strconv.Itoa(len(lines)), strings.Join(lines, "\n")
	case NoticeWeeklySummary:
		var lines []string
		total := 0
		for _, c := range m.Summary {
			total += c.Count
			lines = append(lines, s.groupName(ctx, s.group(ctx, c.GroupID, c.Path))+": "+strconv.Itoa(c.Count))
		}
		vars["Count"], vars["Items"] = strconv.Itoa(total), strings.Join(lines, "\n")
	}
	return vars, nil
}

// group reads a Group for its name; one gone since is named by its path.
func (s *Service) group(ctx context.Context, ref, path string) identity.Group {
	g, err := s.config.Directory.GetGroup(ctx, ref)
	if err != nil {
		return identity.Group{Path: path}
	}
	return g
}

// groupName is how the mails name g: a Team's Leader group by its Team,
// "WEBLAB/LIDERLER · Web Geliştirme" (its own name alone says no more than
// LIDERLER), any other Group as groupName does.
func (s *Service) groupName(ctx context.Context, g identity.Group) string {
	team, role, ok := authz.TeamOf(g.Path, s.config.Rules.PrivilegedRoots, s.config.Rules.LeaderSubgroups)
	if !ok || role == "" {
		return groupName(g)
	}
	teamGroup := s.group(ctx, team, team)
	code := groupCode(teamGroup) + "/" + groupCode(g)
	if turkish := strings.TrimSpace(teamGroup.Attributes["display_name_tr"]); turkish != "" && turkish != groupCode(teamGroup) {
		return code + " · " + turkish
	}
	return code
}

// groupCode is a Group's code: its name, or its path's last segment.
func groupCode(g identity.Group) string {
	if code := strings.TrimSpace(g.Name); code != "" {
		return code
	}
	return g.Path[strings.LastIndex(g.Path, "/")+1:]
}

// groupName is how the mails name a Group, as the membership mails do:
// its code and its Turkish display name.
func groupName(g identity.Group) string {
	code := groupCode(g)
	if turkish := strings.TrimSpace(g.Attributes["display_name_tr"]); turkish != "" && turkish != code {
		return code + " · " + turkish
	}
	return code
}

// Work sends the mails until ctx is cancelled: a pass at once and every
// pollInterval. The returned channel closes once it has stopped. Unless
// the mode is apply it starts nothing.
func (s *Service) Work(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	if !s.applies() {
		close(done)
		return done
	}
	go func() {
		defer close(done)
		ticker := time.NewTicker(pollInterval)
		defer ticker.Stop()
		for {
			if _, err := s.Pass(ctx); err != nil && ctx.Err() == nil {
				s.logf("management term mail: queue: %s", errorCode(err))
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return done
}
