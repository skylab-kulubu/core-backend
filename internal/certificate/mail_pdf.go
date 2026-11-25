package certificate

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync/atomic"
	"time"

	"github.com/skylab-kulubu/core-backend/internal/mail"
)

// MailPDFTimeout bounds reading one certificate's PDF for its mail
// (CERTIFICATE_PDF_ATTACHMENT): storage slower than this, or failing, and
// the mail goes without it.
const MailPDFTimeout = 10 * time.Second

// CertificateAttachmentMailer is a CertificateMailer that can send files
// with the mail (*mail.SkyMail). The PDF goes along only with one.
type CertificateAttachmentMailer interface {
	CertificateMailAttachments(ctx context.Context, idempotencyKey, recipientEmail, fullName string, vars map[string]string, attachments []mail.Attachment) error
}

// Why a mail went without its PDF while the PDF is on: the file is over
// SkyMail's bound, it could not be read, or SkyMail refused the mail with
// it.
const (
	pdfSkipTooLarge = "too_large"
	pdfSkipRead     = "read_failed"
	pdfSkipRefused  = "refused"
)

// mailPDFMetrics count the certificate mails' PDFs on /v1/metrics: how
// many went along, and how many mails went without one and why.
type mailPDFMetrics struct {
	attached atomic.Int64
	tooLarge atomic.Int64
	read     atomic.Int64
	refused  atomic.Int64
}

func (m *mailPDFMetrics) skip(reason string) {
	switch reason {
	case pdfSkipTooLarge:
		m.tooLarge.Add(1)
	case pdfSkipRead:
		m.read.Add(1)
	case pdfSkipRefused:
		m.refused.Add(1)
	}
}

func (m *mailPDFMetrics) prometheus() string {
	return fmt.Sprintf("# TYPE core_certificate_mail_pdf_attached_total counter\ncore_certificate_mail_pdf_attached_total %d\n"+
		"# TYPE core_certificate_mail_pdf_skipped_total counter\n"+
		"core_certificate_mail_pdf_skipped_total{reason=%q} %d\n"+
		"core_certificate_mail_pdf_skipped_total{reason=%q} %d\n"+
		"core_certificate_mail_pdf_skipped_total{reason=%q} %d\n",
		m.attached.Load(), pdfSkipTooLarge, m.tooLarge.Load(), pdfSkipRead, m.read.Load(), pdfSkipRefused, m.refused.Load())
}

// mailAttachments is c's PDF as its mail's attachment, or nil when the mail
// goes without it: the PDF is off, the sender takes no files, the PDF could
// not be read within MailPDFTimeout, or it is over SkyMail's
// mail.MaxAttachmentBytes (512 KB decoded; base64 makes it about 683 KB of
// the request, far under Graph's 4 MB). Each try reads it again from
// storage, the same bytes; SkyMail keeps files out of the Idempotency-Key's
// digest, so a try without it is the same mail. The log names the mail's
// row, never the person.
func (s *service) mailAttachments(ctx context.Context, sender CertificateMailer, m Mail) []mail.Attachment {
	if !s.mailPDFOn {
		return nil
	}
	if _, ok := sender.(CertificateAttachmentMailer); !ok {
		return nil
	}
	readCtx, cancel := context.WithTimeout(ctx, MailPDFTimeout)
	defer cancel()
	// The PDF as GET /v1/certificates/{serial}/pdf reads it: a fresh row,
	// so a certificate revoked since the claim (ErrNotFound) sends no file.
	data, err := s.PDF(readCtx, m.Certificate.Serial)
	if err == nil && len(data) == 0 {
		err = errors.New("empty PDF")
	}
	switch {
	case err != nil:
		s.mailPDF.skip(pdfSkipRead)
		log.Printf("WARN certificate mail %s: PDF not read, the mail goes with its link only (%v)", m.ID, err)
		return nil
	case len(data) > mail.MaxAttachmentBytes:
		s.mailPDF.skip(pdfSkipTooLarge)
		log.Printf("WARN certificate mail %s: the PDF is %d KB, over SkyMail's %d KB; the mail goes with its link only", m.ID, (len(data)+1023)/1024, mail.MaxAttachmentBytes/1024)
		return nil
	}
	return []mail.Attachment{mail.PDFAttachment(CertificatePDFFilename(m.Certificate.Serial), data)}
}

// postMail sends m with attachments: through CertificateMailAttachments
// when there are any, else CertificateMail as before. A send that SkyMail
// took counts the PDF.
func (s *service) postMail(ctx context.Context, sender CertificateMailer, m Mail, attachments []mail.Attachment) error {
	c := m.Certificate
	key := MailIdempotencyKeyPrefix + m.ID.String()
	if len(attachments) == 0 {
		return sender.CertificateMail(ctx, key, c.RecipientEmail, c.RecipientName, certificateMailVars(c))
	}
	err := sender.(CertificateAttachmentMailer).CertificateMailAttachments(ctx, key, c.RecipientEmail, c.RecipientName, certificateMailVars(c), attachments)
	if err == nil {
		s.mailPDF.attached.Add(1)
	}
	return err
}

// CertificatePDFFilename is the name the PDF goes under:
// SKY-LAB-sertifika-<serial>.pdf, ASCII letters, digits and '-' only (never
// the person's name; SkyMail would reduce anything else).
func CertificatePDFFilename(serial string) string {
	clean := strings.Map(func(r rune) rune {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			return r
		}
		return -1
	}, serial)
	if clean == "" {
		return "SKY-LAB-sertifika.pdf"
	}
	return "SKY-LAB-sertifika-" + clean + ".pdf"
}
