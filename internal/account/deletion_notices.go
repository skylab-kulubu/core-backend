package account

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// DeletionDeadline is how long the club has to answer a deletion request:
// thirty days from the application (KVKK art. 13(2)). The received notice
// names its last day.
const DeletionDeadline = 30 * 24 * time.Hour

// deletionNoticeZone is the zone the deadline's day is named in. Turkey has
// kept UTC+3 all year since 2016.
var deletionNoticeZone = time.FixedZone("TRT", 3*60*60)

// DeletionDeadlineDay is the last day of a request made at requestedAt, as
// the received notice and Account Center write it: DD.MM.YYYY in Istanbul.
func DeletionDeadlineDay(requestedAt time.Time) string {
	return requestedAt.Add(DeletionDeadline).In(deletionNoticeZone).Format("02.01.2006")
}

// DeletionNoticeClaims takes a notice in hand once per request, under the
// worker's lease (user.PostgresStore).
type DeletionNoticeClaims interface {
	ClaimDeletionNotice(ctx context.Context, requestID, leaseToken uuid.UUID, notice user.DeletionNotice, at time.Time) (bool, error)
}

// DeletionNoticeMailer sends one notice to one address and keeps no record
// of it (mail.SkyMail, erase_after_send).
type DeletionNoticeMailer interface {
	AccountDeletionNotice(ctx context.Context, templateKey, recipient string, vars map[string]string) error
}

// DeletionNotices are the two mails a deletion request sends the person
// (KVKK art. 13: the application is answered, and its outcome told): that
// the request was received, with its last day, and that the account and its
// data were deleted (docs/account-lifecycle.md, "Deletion notices"). They are
// best effort: a notice that cannot be sent is logged, never retried, and
// the erasure goes on as if it had been.
type DeletionNotices struct {
	Claims DeletionNoticeClaims
	Mailer DeletionNoticeMailer
	// ReceivedTemplateKey and CompletedTemplateKey address the SkyMail
	// templates; an empty key turns that notice off.
	ReceivedTemplateKey  string
	CompletedTemplateKey string
	// Logf writes the one line of each notice; nil is log.Printf.
	Logf func(format string, args ...any)
}

func (n *DeletionNotices) key(notice user.DeletionNotice) string {
	if n == nil || n.Claims == nil || n.Mailer == nil {
		return ""
	}
	switch notice {
	case user.DeletionNoticeReceived:
		return strings.TrimSpace(n.ReceivedTemplateKey)
	case user.DeletionNoticeCompleted:
		return strings.TrimSpace(n.CompletedTemplateKey)
	}
	return ""
}

func (n *DeletionNotices) logf(format string, args ...any) {
	if n.Logf != nil {
		n.Logf(format, args...)
		return
	}
	log.Printf(format, args...)
}

// claim takes the notice in hand for the request. A failed claim is
// logged and sends nothing: the next pass claims it.
func (n *DeletionNotices) claim(ctx context.Context, request user.DeletionRequest, leaseToken uuid.UUID, notice user.DeletionNotice, at time.Time) bool {
	claimed, err := n.Claims.ClaimDeletionNotice(ctx, request.ID, leaseToken, notice, at)
	if err != nil {
		n.logf("account erasure notice request_id=%s notice=%s: claim failed: %v", request.ID, notice, err)
		return false
	}
	return claimed
}

// send mails the notice to each address and writes one line: how many
// went, and the first failure. The line carries the request id, never an
// address, a name or the subject.
func (n *DeletionNotices) send(ctx context.Context, request user.DeletionRequest, notice user.DeletionNotice, emails []string, vars map[string]string) {
	key := n.key(notice)
	sent := 0
	var failures []error
	for _, email := range emails {
		if err := n.Mailer.AccountDeletionNotice(ctx, key, email, vars); err != nil {
			failures = append(failures, err)
			continue
		}
		sent++
	}
	switch {
	case len(emails) == 0:
		n.logf("account erasure notice request_id=%s notice=%s: no address left to send it to", request.ID, notice)
	case len(failures) == 0:
		n.logf("account erasure notice request_id=%s notice=%s sent=%d", request.ID, notice, sent)
	default:
		n.logf("account erasure notice request_id=%s notice=%s sent=%d failed=%d: %v", request.ID, notice, sent, len(failures), errors.Join(failures...))
	}
}

// selfRequested is a request the person made: through Account Center, or
// for themselves. One an administrator made for someone else gets no
// received notice: the person asked for nothing.
func selfRequested(request user.DeletionRequest) bool {
	return request.RequestedBy == nil || *request.RequestedBy == request.SubjectID
}

// noticeReceived sends the received notice once SkyMail has erased the
// person (erase_skymail checkpointed, in this pass or an earlier one), at
// most once per request: the claim comes first. Sent after that step, the
// notice is the one SkyMail keeps no record of (erase_after_send) and no
// erasure waits for. Tried once per pass.
func (w *Worker) noticeReceived(ctx context.Context, request user.DeletionRequest, leaseToken uuid.UUID, completed map[user.DeletionStep]bool, services *ServiceErasure, pass *erasurePass) {
	notices := w.config.Notices
	if pass.receivedNoticeTried || !completed[user.DeletionStepEraseSkyMail] || notices.key(user.DeletionNoticeReceived) == "" || !selfRequested(request) {
		return
	}
	pass.receivedNoticeTried = true
	if !notices.claim(ctx, request, leaseToken, user.DeletionNoticeReceived, w.config.Now()) {
		return
	}
	emails, err := w.passAddresses(ctx, request, pass, services.Addresses)
	if err != nil {
		notices.logf("account erasure notice request_id=%s notice=%s: addresses unavailable, not sent: %v", request.ID, user.DeletionNoticeReceived, err)
		return
	}
	sendCtx, cancel := context.WithTimeout(ctx, w.config.StepTimeout)
	defer cancel()
	notices.send(sendCtx, request, user.DeletionNoticeReceived, emails, map[string]string{"Deadline": DeletionDeadlineDay(request.CreatedAt)})
}

// completedNoticeAddresses reads, before delete_identity, the addresses
// the completed notice goes to: after it Keycloak no longer knows the
// person and core's row is anonymized. A failed read is logged and leaves
// none; the erasure goes on.
func (w *Worker) completedNoticeAddresses(ctx context.Context, request user.DeletionRequest, pass *erasurePass) {
	if w.config.Notices.key(user.DeletionNoticeCompleted) == "" || pass.completedNoticeRead {
		return
	}
	pass.completedNoticeRead = true
	emails, err := w.passAddresses(ctx, request, pass, w.services.Addresses)
	if err != nil {
		w.config.Notices.logf("account erasure notice request_id=%s notice=%s: addresses unavailable: %v", request.ID, user.DeletionNoticeCompleted, err)
		return
	}
	pass.completedNoticeEmails = emails
}

// claimCompleted takes the completed notice in hand before the request is
// completed: true sends it once the completion is written.
func (w *Worker) claimCompleted(ctx context.Context, request user.DeletionRequest, leaseToken uuid.UUID) bool {
	notices := w.config.Notices
	if notices.key(user.DeletionNoticeCompleted) == "" {
		return false
	}
	return notices.claim(ctx, request, leaseToken, user.DeletionNoticeCompleted, w.config.Now())
}

// noticeCompleted sends the completed notice to the addresses read before
// the identity was deleted.
func (w *Worker) noticeCompleted(ctx context.Context, request user.DeletionRequest, pass *erasurePass) {
	sendCtx, cancel := context.WithTimeout(ctx, w.config.StepTimeout)
	defer cancel()
	w.config.Notices.send(sendCtx, request, user.DeletionNoticeCompleted, pass.completedNoticeEmails, nil)
}

// String keeps a DeletionNotices out of a log line whole.
func (n *DeletionNotices) String() string {
	if n == nil {
		return "off"
	}
	return fmt.Sprintf("received %q, completed %q", n.key(user.DeletionNoticeReceived), n.key(user.DeletionNoticeCompleted))
}
