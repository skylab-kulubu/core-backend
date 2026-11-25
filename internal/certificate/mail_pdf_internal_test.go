package certificate

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/mail"
)

// revokedFileMailer takes files with the mail and records each send's.
type revokedFileMailer struct{ files [][]mail.Attachment }

func (*revokedFileMailer) Certificate(context.Context, string, string, map[string]string) {}

func (m *revokedFileMailer) CertificateMail(context.Context, string, string, string, map[string]string) error {
	m.files = append(m.files, nil)
	return nil
}

func (m *revokedFileMailer) CertificateMailAttachments(_ context.Context, _, _, _ string, _ map[string]string, attachments []mail.Attachment) error {
	m.files = append(m.files, attachments)
	return nil
}

// A certificate revoked after its mail was claimed: the claim's snapshot
// still says active, but the PDF is read from a fresh row, which refuses
// it (ErrNotFound). The mail carries no file.
func TestMailAttachmentsReadAFreshRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	cert, err := store.Create(ctx, Certificate{ID: uuid.New(), EventID: uuid.New(), TicketID: uuid.New(), Serial: "0A1B2C", RecipientName: "Ada Test", RecipientEmail: "ada@example.com", EventName: "ARTLAB"}, []byte("%PDF-1.4 artlab"))
	if err != nil {
		t.Fatal(err)
	}
	mailer := &revokedFileMailer{}
	svc := &service{store: store, mail: mailer, mailPDFOn: true}
	claimed := Mail{ID: uuid.New(), CertificateID: cert.ID, EventID: cert.EventID, Attempts: 1, FirstTriedAt: time.Now().UTC(), Certificate: cert}

	if got := svc.mailAttachments(ctx, mailer, claimed); len(got) != 1 || got[0].Filename != "SKY-LAB-sertifika-0A1B2C.pdf" {
		t.Fatalf("active certificate: %+v", got)
	}
	if _, err := store.Revoke(ctx, cert.Serial); err != nil {
		t.Fatal(err)
	}
	if got := svc.mailAttachments(ctx, mailer, claimed); got != nil {
		t.Fatalf("revoked certificate went with its PDF: %+v", got)
	}
	if m := svc.mailPDF.prometheus(); !strings.Contains(m, `core_certificate_mail_pdf_skipped_total{reason="read_failed"} 1`) {
		t.Fatalf("metrics\n%s", m)
	}
}
