package ticketnotify_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/skylab-kulubu/core-backend/internal/mail"
	"github.com/skylab-kulubu/core-backend/internal/ticketnotify"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// fakePDF is the ticket page's PDF (ticketpage.Service.PDF).
type fakePDF struct {
	keys      []string
	data      []byte
	err       error
	deadlines []time.Duration
	// each runs on every call (to move the clock).
	each func()
}

func (p *fakePDF) PDF(ctx context.Context, key string) (string, []byte, error) {
	p.keys = append(p.keys, key)
	if deadline, ok := ctx.Deadline(); ok {
		p.deadlines = append(p.deadlines, time.Until(deadline))
	}
	if p.each != nil {
		p.each()
	}
	if p.err != nil {
		return "", nil, p.err
	}
	return "bilet-kodlama-gecesi.pdf", p.data, nil
}

// withPDF is e's worker with TICKET_PDF_ATTACHMENT on; logs go to the
// buffer it answers.
func (e *env) withPDF(t *testing.T, pdf *fakePDF) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	worker, err := ticketnotify.NewService(ticketnotify.Config{
		Queue: e.queue, Tickets: e.tickets, Events: e.events, Users: e.users, Mail: e.mail, PDF: pdf,
		Now:    func() time.Time { return e.now },
		Logger: log.New(&logs, "", 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	e.worker = worker
	return &logs
}

var aPDF = []byte("%PDF-1.7\nbilet")

func TestPDFGoesWithTheMailOfAMemberAndAGuest(t *testing.T) {
	e := newEnv(t)
	pdf := &fakePDF{data: aPDF}
	e.withPDF(t, pdf)
	member := e.member(user.AccountActive)
	member.AccessKey = "member-key"
	e.tickets[member.ID] = member
	guest := e.guest("can@example.com")
	guest.AccessKey = "guest-key"
	e.tickets[guest.ID] = guest
	if r := e.pass(t); r.Sent != 2 || r.PDFSkipped != 0 {
		t.Fatalf("report %+v, want two sent with the PDF", r)
	}
	if strings.Join(pdf.keys, ",") != "member-key,guest-key" {
		t.Fatalf("PDFs drawn for %v, want each Ticket's own key", pdf.keys)
	}
	for i, c := range e.mail.calls {
		if len(c.attachments) != 1 {
			t.Fatalf("call %d: attachments %v, want one", i, c.attachments)
		}
		a := c.attachments[0]
		if a.Filename != "bilet-kodlama-gecesi.pdf" || a.ContentType != "application/pdf" || a.ContentBase64 != base64.StdEncoding.EncodeToString(aPDF) {
			t.Fatalf("call %d: attachment %+v", i, a)
		}
	}
	if pdf.deadlines[0] <= 0 || pdf.deadlines[0] > ticketnotify.PDFTimeout {
		t.Fatalf("PDF deadline %v, want at most %v", pdf.deadlines[0], ticketnotify.PDFTimeout)
	}
}

func TestPDFOffSendsNoFile(t *testing.T) {
	e := newEnv(t)
	e.member(user.AccountActive)
	if r := e.pass(t); r.Sent != 1 || r.PDFSkipped != 0 {
		t.Fatalf("report %+v", r)
	}
	if e.mail.calls[0].attachments != nil {
		t.Fatalf("attachments %v with the switch off", e.mail.calls[0].attachments)
	}
}

// Gotenberg failing or too slow: the mail goes without the PDF (its link, QR
// and code are in it), counted pdf_skipped; the rest of the pass does not
// wait on Gotenberg again.
func TestGotenbergFailureSendsWithoutThePDF(t *testing.T) {
	for name, cause := range map[string]error{"error": errors.New("ticketpage: unavailable: renderer: 503"), "timeout": context.DeadlineExceeded} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			pdf := &fakePDF{data: aPDF, err: cause}
			logs := e.withPDF(t, pdf)
			e.member(user.AccountActive)
			e.guest("can@example.com")
			r := e.pass(t)
			if r.Sent != 2 || r.PDFSkipped != 2 || r.Retried != 0 {
				t.Fatalf("report %+v, want both sent without the PDF", r)
			}
			if len(pdf.keys) != 1 {
				t.Fatalf("PDF drawn %d times, want once: the pass stops asking after a failure", len(pdf.keys))
			}
			for i, c := range e.mail.calls {
				if c.attachments != nil {
					t.Fatalf("call %d: attachments %v", i, c.attachments)
				}
			}
			if strings.Contains(logs.String(), "can@example.com") || !strings.Contains(logs.String(), e.ev.ID.String()) {
				t.Fatalf("log %q: the Event, never the holder", logs.String())
			}
			// The next pass tries Gotenberg again.
			pdf.err = nil
			e.member(user.AccountActive)
			if r := e.pass(t); r.Sent != 1 || r.PDFSkipped != 0 || len(e.mail.calls[2].attachments) != 1 {
				t.Fatalf("next pass: report %+v, attachments %v", r, e.mail.calls[2].attachments)
			}
		})
	}
}

func TestPDFOverSkyMailsBoundGoesWithoutAndLogsNoPerson(t *testing.T) {
	e := newEnv(t)
	big := append(append([]byte{}, aPDF...), make([]byte, mail.MaxAttachmentBytes)...)
	pdf := &fakePDF{data: big}
	logs := e.withPDF(t, pdf)
	e.guest("can@example.com")
	e.member(user.AccountActive)
	if r := e.pass(t); r.Sent != 2 || r.PDFSkipped != 2 {
		t.Fatalf("report %+v", r)
	}
	if e.mail.calls[0].attachments != nil || e.mail.calls[1].attachments != nil {
		t.Fatal("a PDF over 512 KB was sent")
	}
	if len(pdf.keys) != 2 {
		t.Fatalf("PDF drawn %d times: a size is no Gotenberg failure, the next Ticket's is drawn", len(pdf.keys))
	}
	line := logs.String()
	if !strings.Contains(line, "over SkyMail's 512 KB") || strings.Contains(line, "can@example.com") || strings.Contains(line, "Can") {
		t.Fatalf("log %q", line)
	}
}

// A retry draws the PDF again and keeps the key: SkyMail answers the first
// try's replay and keeps its file.
func TestRetryDrawsThePDFAgainUnderTheSameKey(t *testing.T) {
	e := newEnv(t)
	pdf := &fakePDF{data: aPDF}
	e.withPDF(t, pdf)
	tk := e.member(user.AccountActive)
	e.mail.err = &mail.SendError{Status: 503}
	if r := e.pass(t); r.Retried != 1 {
		t.Fatalf("report %+v", r)
	}
	e.now = e.now.Add(ticketnotify.RetryFirst)
	e.mail.err = nil
	if r := e.pass(t); r.Sent != 1 {
		t.Fatalf("report %+v", r)
	}
	if len(pdf.keys) != 2 {
		t.Fatalf("PDF drawn %d times, want again on the retry", len(pdf.keys))
	}
	if c := e.mail.calls; len(c) != 2 || c[0].key != ticketnotify.IdempotencyKey(tk.ID) || c[1].key != c[0].key || len(c[1].attachments) != 1 {
		t.Fatalf("calls %+v", c)
	}
}

// SkyMail refusing the body with the file (400 attachment.*) is no reason to
// lose the mail: it goes again at once without it, under the same key.
func TestRefusedAttachmentSendsTheMailWithoutIt(t *testing.T) {
	e := newEnv(t)
	e.withPDF(t, &fakePDF{data: aPDF})
	tk := e.member(user.AccountActive)
	e.mail.errs = []error{&mail.SendError{Status: 400, Code: "attachment.too_large"}}
	if r := e.pass(t); r.Sent != 1 || r.Rejected != 0 || r.PDFSkipped != 1 {
		t.Fatalf("report %+v", r)
	}
	c := e.mail.calls
	if len(c) != 2 || len(c[0].attachments) != 1 || c[1].attachments != nil || c[1].key != ticketnotify.IdempotencyKey(tk.ID) || c[0].key != c[1].key {
		t.Fatalf("calls %+v, want the same key with, then without the file", c)
	}
}

// The panel's "Bileti gönder" queues the row again with its resend number:
// the same worker sends it, so it carries the PDF too, under …:r<n>.
func TestResendCarriesThePDF(t *testing.T) {
	e := newEnv(t)
	e.withPDF(t, &fakePDF{data: aPDF})
	tk := e.member(user.AccountActive)
	e.queue.mu.Lock()
	e.queue.rows[0].Resend = 1
	e.queue.mu.Unlock()
	if r := e.pass(t); r.Sent != 1 {
		t.Fatalf("report %+v", r)
	}
	if c := e.mail.calls[0]; c.key != ticketnotify.ResendKey(tk.ID, 1) || len(c.attachments) != 1 {
		t.Fatalf("call %+v", c)
	}
}

// While the PDF goes along a row is started only with a whole PDFTimeout and
// a send left of the lease: the rest wait for it to run out, and no other
// core takes a row this one is still sending.
func TestSlowPDFsLeaveTheRestForTheNextClaim(t *testing.T) {
	e := newEnv(t)
	pdf := &fakePDF{data: aPDF}
	pdf.each = func() { e.now = e.now.Add(150 * time.Second) }
	e.withPDF(t, pdf)
	e.member(user.AccountActive)
	e.member(user.AccountActive)
	start := e.now
	if r := e.pass(t); r.Sent != 1 {
		t.Fatalf("report %+v, want one sent", r)
	}
	if e.queue.len() != 1 {
		t.Fatalf("rows %d, want the second left", e.queue.len())
	}
	// Still held: the lease is longer than PDFTimeout plus a send.
	e.now = start.Add(ticketnotify.PDFTimeout + time.Minute)
	pdf.each = nil
	if r := e.pass(t); r.Sent != 0 {
		t.Fatalf("report %+v: taken while still held", r)
	}
	e.now = start.Add(10 * time.Minute)
	if r := e.pass(t); r.Sent != 1 || e.queue.len() != 0 {
		t.Fatalf("report %+v, rows %d", r, e.queue.len())
	}
}
