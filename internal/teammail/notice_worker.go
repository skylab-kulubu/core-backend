package teammail

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/mail"
)

// noticePass drops the notices past GiveUpAfter, then handles the due ones
// until none is due: a change is read for its recipients and replaced by a
// notice to each, which the same pass then sends.
func (s *Service) noticePass(ctx context.Context) (NoticeReport, error) {
	var report NoticeReport
	queue := s.notice.Queue
	expired, err := queue.DropBefore(ctx, s.now().UTC().Add(-GiveUpAfter))
	if err != nil {
		return report, err
	}
	report.Expired = expired
	s.noticeOutcome("expired", expired)
	for range maxSends {
		claimed, err := queue.Claim(ctx, s.now().UTC(), lease, 1)
		if err != nil {
			return report, err
		}
		if len(claimed) == 0 {
			break
		}
		n := claimed[0]
		outcome, retry, err := s.handleNotice(ctx, n)
		if err != nil {
			return report, err
		}
		if outcome == outcomeInterrupted {
			return report, ctx.Err()
		}
		s.noticeOutcome(outcome, 1)
		switch outcome {
		case "read", "no_recipients":
			report.Read++
		case "sent":
			report.Sent++
		case "rejected":
			report.Rejected++
		case "failed":
			report.Failed++
		default:
			report.Skipped++
		}
		writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
		switch {
		case outcome == "read":
			// Expand already replaced the row.
		case retry:
			err = queue.Retry(writeCtx, n, s.now().UTC().Add(backoff(n.Attempts+1)))
		default:
			err = queue.Complete(writeCtx, n)
		}
		cancel()
		if err != nil {
			return report, err
		}
		if ctx.Err() != nil {
			return report, ctx.Err()
		}
	}
	backlog, oldest, err := queue.Backlog(ctx, s.now().UTC())
	if err != nil {
		return report, err
	}
	s.count(func() { s.notices.backlog, s.notices.oldest = backlog, oldest })
	return report, nil
}

// handleNotice reads a change for its recipients or sends a notice to one,
// and names the outcome: read (the change now waits as one notice per
// recipient), no_recipients, sent, skipped_<reason>, rejected, failed (and
// retry) or interrupted. err is the queue's.
func (s *Service) handleNotice(parent context.Context, n Notice) (outcome string, retry bool, err error) {
	ctx, cancel := context.WithTimeout(parent, sendTimeout)
	defer cancel()
	if !s.noticeOn() {
		return "skipped_" + string(SkipKindOff), false, nil
	}
	failed := func() (string, bool, error) {
		if parent.Err() != nil {
			return outcomeInterrupted, false, nil
		}
		return "failed", true, nil
	}
	if n.RecipientID == nil {
		recipients, count, err := s.noticeRecipients(ctx, n)
		if err != nil {
			return failed()
		}
		if len(recipients) == 0 {
			return "no_recipients", false, nil
		}
		writeCtx, cancelWrite := context.WithTimeout(context.WithoutCancel(parent), writeTimeout)
		defer cancelWrite()
		added, err := s.notice.Queue.Expand(writeCtx, n, recipients, count, s.now().UTC())
		if err != nil {
			return "", false, err
		}
		s.count(func() { s.notices.recipients += int64(added) })
		return "read", false, nil
	}
	recipient, skip, err := s.people.Recipient(ctx, *n.RecipientID)
	if err != nil {
		return failed()
	}
	if skip != "" {
		return "skipped_" + string(skip), false, nil
	}
	err = s.notice.Mail.PrivilegeNotice(ctx, recipient.Email, recipient.FullName, s.noticeVars(ctx, n))
	var refused *mail.SendError
	switch {
	case err == nil:
		return "sent", false, nil
	case errors.Is(err, mail.ErrTemplateUnconfigured):
		return "skipped_" + string(SkipKindOff), false, nil
	case errors.As(err, &refused) && refused.Permanent():
		return "rejected", false, nil
	default:
		return failed()
	}
}

func (s *Service) noticeOutcome(outcome string, n int) {
	if n == 0 {
		return
	}
	s.count(func() { s.notices.outcomes[outcome] += int64(n) })
}

// noticeOutcomeNames are the outcomes /v1/metrics always lists.
var noticeOutcomeNames = []string{
	"read", "no_recipients", "sent", "failed", "rejected", "expired",
	"skipped_" + string(SkipInactive), "skipped_" + string(SkipNoEmail), "skipped_" + string(SkipKindOff),
}

func noticeEnabledMetric(on bool) string {
	value := 0
	if on {
		value = 1
	}
	return fmt.Sprintf("# TYPE skylab_privilege_notice_mail_enabled gauge\nskylab_privilege_notice_mail_enabled %d\n", value)
}

// noticeMetrics are the privilege notice's counters; the caller holds s.mu.
// No person, address, Group or role appears.
func (s *Service) noticeMetrics() string {
	var out strings.Builder
	out.WriteString(noticeEnabledMetric(s.noticeOn()))
	out.WriteString("# TYPE skylab_privilege_notice_mail_total counter\n")
	for _, outcome := range noticeOutcomeNames {
		fmt.Fprintf(&out, "skylab_privilege_notice_mail_total{outcome=%q} %d\n", outcome, s.notices.outcomes[outcome])
	}
	out.WriteString("# TYPE skylab_privilege_notice_mail_enqueued_total counter\n")
	for _, event := range []NoticeEvent{NoticeMemberAdded, NoticeMemberRemoved, NoticeRolesChanged} {
		fmt.Fprintf(&out, "skylab_privilege_notice_mail_enqueued_total{event=%q} %d\n", event, s.notices.enqueued[event])
	}
	fmt.Fprintf(&out, "# TYPE skylab_privilege_notice_mail_recipients_total counter\nskylab_privilege_notice_mail_recipients_total %d\n", s.notices.recipients)
	fmt.Fprintf(&out, "# TYPE skylab_privilege_notice_mail_queue_errors_total counter\nskylab_privilege_notice_mail_queue_errors_total %d\n", s.notices.queueErrors)
	fmt.Fprintf(&out, "# TYPE skylab_privilege_notice_mail_precheck_errors_total counter\nskylab_privilege_notice_mail_precheck_errors_total %d\n", s.notices.precheckErrors)
	fmt.Fprintf(&out, "# TYPE skylab_privilege_notice_mail_count_errors_total counter\nskylab_privilege_notice_mail_count_errors_total %d\n", s.notices.countErrors)
	fmt.Fprintf(&out, "# TYPE skylab_privilege_notice_mail_backlog gauge\nskylab_privilege_notice_mail_backlog %d\n", s.notices.backlog)
	fmt.Fprintf(&out, "# TYPE skylab_privilege_notice_mail_oldest_age_seconds gauge\nskylab_privilege_notice_mail_oldest_age_seconds %d\n", int64(s.notices.oldest.Seconds()))
	return out.String()
}

var _ identity.GroupRolesNotifier = (*Service)(nil)
