package certificate_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/certificate"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
)

// The certificate mail queue on PostgreSQL (docs/certificate-mail.md): one
// issued mail and one queued mail per certificate; a claim reads the
// certificate and counts the try; only the claim's holder settles it; the
// latest mail is the certificate's state; the row goes with its
// certificate.
func TestPostgresCertificateMailQueue(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	start := time.Now().Add(-48 * time.Hour)
	ev, err := event.NewPostgresStore(pool).Create(ctx, event.Event{Name: "ARTLAB", Location: "YTÜ", OwnerTeam: "ARTLAB", StartDate: &start})
	if err != nil {
		t.Fatal(err)
	}
	ticketID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO tickets (id, event_id, ticket_type, guest_first_name, guest_last_name, guest_email) VALUES ($1, $2, 'GUEST', 'Ada', 'Lovelace', 'ada@example.com')`, ticketID, ev.ID); err != nil {
		t.Fatal(err)
	}
	store := certificate.NewPostgresStore(pool)
	cert, err := store.Create(ctx, certificate.Certificate{EventID: ev.ID, TicketID: ticketID, Serial: "ABCDEF0123456789ABCDEF0123456789", RecipientName: "Ada Lovelace", RecipientEmail: "ada@example.com", EventName: "ARTLAB", OwnerTeam: "ARTLAB", VerifyURL: "https://skyl.app/c/ABCDEF0123456789ABCDEF0123456789"}, []byte("%PDF"))
	if err != nil {
		t.Fatal(err)
	}
	enqueue := func(reason string) bool {
		t.Helper()
		written, err := store.EnqueueMail(ctx, certificate.Mail{ID: uuid.New(), CertificateID: cert.ID, EventID: ev.ID, Reason: reason})
		if err != nil {
			t.Fatal(err)
		}
		return written
	}
	if !enqueue(certificate.MailReasonIssued) {
		t.Fatal("the issued mail was not written")
	}
	if enqueue(certificate.MailReasonIssued) || enqueue(certificate.MailReasonResend) {
		t.Fatal("a second mail was queued beside a queued one")
	}

	claimed, err := store.ClaimMails(ctx, 10, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim %v %+v", err, claimed)
	}
	m := claimed[0]
	if m.Attempts != 1 || m.Reason != certificate.MailReasonIssued || m.ClaimToken == uuid.Nil || m.FirstTriedAt.IsZero() ||
		m.Certificate.RecipientEmail != "ada@example.com" || m.Certificate.Serial != cert.Serial || m.Certificate.RevokedAt != nil {
		t.Fatalf("claimed %+v", m)
	}
	if again, err := store.ClaimMails(ctx, 10, time.Minute); err != nil || len(again) != 0 {
		t.Fatalf("a claimed mail was claimed again: %v %+v", err, again)
	}
	if err := store.FinishMail(ctx, m.ID, uuid.New(), certificate.MailSent, "", time.Now()); !errors.Is(err, certificate.ErrLeaseLost) {
		t.Fatalf("a stranger settled the mail: %v", err)
	}
	// SkyMail down: back in the queue at retryAt, then the lease is free.
	if err := store.FinishMail(ctx, m.ID, m.ClaimToken, certificate.MailQueued, "skymail_503", time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	states, err := store.MailStates(ctx, ev.ID)
	if err != nil || states[cert.Serial].Status != certificate.MailQueued || states[cert.Serial].ErrorCode != "skymail_503" {
		t.Fatalf("states %v %+v", err, states)
	}
	claimed, err = store.ClaimMails(ctx, 10, time.Minute)
	if err != nil || len(claimed) != 1 || claimed[0].Attempts != 2 || !claimed[0].FirstTriedAt.Equal(m.FirstTriedAt) {
		t.Fatalf("second claim %v %+v (first try %v)", err, claimed, m.FirstTriedAt)
	}
	if err := store.RenewMailLease(ctx, claimed[0].ID, claimed[0].ClaimToken, time.Minute); err != nil {
		t.Fatalf("holder renews: %v", err)
	}
	if err := store.RenewMailLease(ctx, claimed[0].ID, uuid.New(), time.Minute); !errors.Is(err, certificate.ErrLeaseLost) {
		t.Fatalf("a stranger renewed: %v", err)
	}
	if err := store.FinishMail(ctx, claimed[0].ID, claimed[0].ClaimToken, certificate.MailSent, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishMail(ctx, claimed[0].ID, claimed[0].ClaimToken, certificate.MailFailed, "late", time.Now()); !errors.Is(err, certificate.ErrLeaseLost) {
		t.Fatalf("a settled mail was settled again: %v", err)
	}
	states, _ = store.MailStates(ctx, ev.ID)
	if got := states[cert.Serial]; got.Status != certificate.MailSent || got.SentAt == nil || got.ErrorCode != "" || got.Attempts != 2 {
		t.Fatalf("sent %+v", got)
	}

	// A resend is a new row and the latest state; the issued one stays one.
	if !enqueue(certificate.MailReasonResend) || enqueue(certificate.MailReasonIssued) {
		t.Fatal("resend after the sent mail")
	}
	states, _ = store.MailStates(ctx, ev.ID)
	if got := states[cert.Serial]; got.Status != certificate.MailQueued || got.Attempts != 0 {
		t.Fatalf("latest after resend %+v", got)
	}
	// A lease that ran out frees the mail for the next claim.
	claimed, _ = store.ClaimMails(ctx, 10, time.Minute)
	if len(claimed) != 1 {
		t.Fatalf("resend claim %+v", claimed)
	}
	if _, err := pool.Exec(ctx, `UPDATE certificate_mails SET claimed_until = now() - interval '1 second' WHERE id = $1`, claimed[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := store.RenewMailLease(ctx, claimed[0].ID, claimed[0].ClaimToken, time.Minute); !errors.Is(err, certificate.ErrLeaseLost) {
		t.Fatalf("a lease that ran out was renewed: %v", err)
	}
	reclaimed, _ := store.ClaimMails(ctx, 10, time.Minute)
	if len(reclaimed) != 1 || reclaimed[0].ID != claimed[0].ID || reclaimed[0].ClaimToken == claimed[0].ClaimToken {
		t.Fatalf("reclaim %+v", reclaimed)
	}
	if err := store.FinishMail(ctx, claimed[0].ID, claimed[0].ClaimToken, certificate.MailSent, "", time.Now()); !errors.Is(err, certificate.ErrLeaseLost) {
		t.Fatalf("the earlier holder settled the mail: %v", err)
	}

	// Bulk resend: one statement, the waiting one skipped; "all" once per
	// cooldown, "failed" takes the failed alone.
	if err := store.FinishMail(ctx, reclaimed[0].ID, reclaimed[0].ClaimToken, certificate.MailFailed, "skymail_400", time.Now()); err != nil {
		t.Fatal(err)
	}
	got, err := store.ResendEventMails(ctx, ev.ID, certificate.ResendFailed, nil, time.Minute)
	if err != nil || got.Queued != 1 || got.AlreadyQueued != 0 {
		t.Fatalf("resend failed %v %+v", err, got)
	}
	if got, err := store.ResendEventMails(ctx, ev.ID, certificate.ResendFailed, nil, time.Minute); err != nil || got.Queued != 0 || got.AlreadyQueued != 0 {
		t.Fatalf("resend failed again %v %+v", err, got)
	}
	if got, err := store.ResendEventMails(ctx, ev.ID, certificate.ResendAll, nil, time.Minute); err != nil || got.Queued != 0 || got.AlreadyQueued != 1 {
		t.Fatalf("resend all %v %+v", err, got)
	}
	if _, err := store.ResendEventMails(ctx, ev.ID, certificate.ResendAll, nil, time.Minute); !errors.Is(err, certificate.ErrResendTooSoon) {
		t.Fatalf("resend all twice: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE certificate_mail_resend_all SET last_at = now() - interval '2 minutes'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ResendEventMails(ctx, ev.ID, certificate.ResendAll, nil, time.Minute); err != nil {
		t.Fatalf("resend all after the cooldown: %v", err)
	}
	var scope string
	if err := pool.QueryRow(ctx, `SELECT resend_scope FROM certificate_mails WHERE status = 'queued'`).Scan(&scope); err != nil || scope != "failed" {
		t.Fatalf("scope %q %v", scope, err)
	}

	// The rows go with their certificate.
	if _, err := pool.Exec(ctx, `DELETE FROM certificates WHERE id = $1`, cert.ID); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM certificate_mails`).Scan(&left); err != nil || left != 0 {
		t.Fatalf("rows left %d %v", left, err)
	}
}
