package certificate_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/certificate"
	"github.com/skylab-kulubu/core-backend/internal/mail"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

// fileMailer is a queueMailer that also takes files with the mail
// (certificate.CertificateAttachmentMailer). withFile is what it says to a
// send with files, before answer; files are each send's attachments, nil
// for one without.
type fileMailer struct {
	queueMailer
	fileMu   sync.Mutex
	withFile error
	files    [][]mail.Attachment
	fileKeys []string
}

func (m *fileMailer) CertificateMail(ctx context.Context, key, email, name string, vars map[string]string) error {
	m.fileMu.Lock()
	m.files = append(m.files, nil)
	m.fileKeys = append(m.fileKeys, key)
	m.fileMu.Unlock()
	return m.queueMailer.CertificateMail(ctx, key, email, name, vars)
}

func (m *fileMailer) CertificateMailAttachments(ctx context.Context, key, email, name string, vars map[string]string, attachments []mail.Attachment) error {
	m.fileMu.Lock()
	m.files = append(m.files, attachments)
	m.fileKeys = append(m.fileKeys, key)
	refuse := m.withFile
	m.fileMu.Unlock()
	if refuse != nil {
		return refuse
	}
	return m.queueMailer.CertificateMail(ctx, key, email, name, vars)
}

func (m *fileMailer) sends() ([][]mail.Attachment, []string) {
	m.fileMu.Lock()
	defer m.fileMu.Unlock()
	return append([][]mail.Attachment(nil), m.files...), append([]string(nil), m.fileKeys...)
}

// brokenArtifacts keeps what is put but cannot read it back.
type brokenArtifacts struct{}

func (brokenArtifacts) Put(context.Context, string, []byte, media.BlobMetadata) error { return nil }
func (brokenArtifacts) Read(context.Context, string) ([]byte, error) {
	return nil, errors.New("storage down")
}

func metricsOf(t *testing.T, svc certificate.Service) string {
	t.Helper()
	m, ok := svc.(interface{ Prometheus() string })
	if !ok {
		t.Fatal("no metrics")
	}
	return m.Prometheus()
}

func wantPDF(t *testing.T, files []mail.Attachment, serials map[string]bool, pdf []byte) {
	t.Helper()
	if len(files) != 1 {
		t.Fatalf("attachments %+v", files)
	}
	f := files[0]
	data, err := base64.StdEncoding.DecodeString(f.ContentBase64)
	if err != nil || !bytes.Equal(data, pdf) || f.ContentType != "application/pdf" {
		t.Fatalf("attachment %s %q %v", f.ContentType, data, err)
	}
	serial := strings.TrimSuffix(strings.TrimPrefix(f.Filename, "SKY-LAB-sertifika-"), ".pdf")
	if !serials[serial] {
		t.Fatalf("file name %q", f.Filename)
	}
}

func serialsOf(f mailFixture) map[string]bool {
	out := map[string]bool{}
	for _, c := range f.certs {
		out[c.Serial] = true
	}
	return out
}

// With CERTIFICATE_PDF_ATTACHMENT on, each queued mail carries its
// certificate's PDF; a try SkyMail did not answer is tried again with the
// same file under the same key; "yeniden gönder" carries it too.
func TestCertificateMailCarriesThePDF(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mailer := &fileMailer{}
	f := mailSetupWith(t, mailer, &recRender{}, certificate.Options{MailPDF: true})
	pdf := []byte("%PDF-1.4 artlab")

	f.mailer.say(&mail.SendError{Status: http.StatusServiceUnavailable})
	if n := f.process(t); n != 2 {
		t.Fatalf("claimed %d", n)
	}
	f.mailer.say(nil)
	f.store.MakeMailsDue()
	if n := f.process(t); n != 2 {
		t.Fatalf("claimed %d", n)
	}
	files, keys := mailer.sends()
	if len(files) != 4 {
		t.Fatalf("sends %d", len(files))
	}
	byKey := map[string]string{}
	for i, got := range files {
		wantPDF(t, got, serialsOf(f), pdf)
		if name, ok := byKey[keys[i]]; ok && name != got[0].Filename {
			t.Fatalf("key %s went with %s and %s", keys[i], name, got[0].Filename)
		}
		byKey[keys[i]] = got[0].Filename
	}
	if len(byKey) != 2 {
		t.Fatalf("keys %v", keys)
	}
	for serial, state := range f.states(t) {
		if state.Status != certificate.MailSent {
			t.Fatalf("%s: %+v", serial, state)
		}
	}

	if _, err := f.svc.ResendMail(ctx, f.leader, f.certs[0].Serial); err != nil {
		t.Fatal(err)
	}
	f.process(t)
	files, keys = mailer.sends()
	if len(files) != 5 || byKey[keys[4]] != "" {
		t.Fatalf("resend: %d sends, key %s", len(files), keys[4])
	}
	wantPDF(t, files[4], map[string]bool{f.certs[0].Serial: true}, pdf)
	if m := metricsOf(t, f.svc); !strings.Contains(m, "core_certificate_mail_pdf_attached_total 3\n") {
		t.Fatalf("metrics\n%s", m)
	}
}

// Off, the mail goes as before: no file, through CertificateMail.
func TestCertificateMailWithoutThePDFWhenOff(t *testing.T) {
	t.Parallel()
	mailer := &fileMailer{}
	f := mailSetupWith(t, mailer, &recRender{}, certificate.Options{})
	if n := f.process(t); n != 2 {
		t.Fatalf("claimed %d", n)
	}
	files, _ := mailer.sends()
	if len(files) != 2 || files[0] != nil || files[1] != nil {
		t.Fatalf("sends %+v", files)
	}
	if m := metricsOf(t, f.svc); !strings.Contains(m, "core_certificate_mail_pdf_attached_total 0\n") {
		t.Fatalf("metrics\n%s", m)
	}
}

// A PDF over SkyMail's 512 KB, one storage cannot read, or a mail SkyMail
// refuses with the file: the mail still goes, without it (its verify link
// is in it), under its own key, and the metric says why.
func TestCertificateMailGoesWithoutThePDFItCannotCarry(t *testing.T) {
	t.Parallel()
	big := append([]byte("%PDF-1.4 "), bytes.Repeat([]byte("x"), mail.MaxAttachmentBytes)...)
	cases := []struct {
		name     string
		render   *recRender
		opts     certificate.Options
		withFile error
		reason   string
		sends    int
	}{
		{name: "too large", render: &recRender{pdf: big}, opts: certificate.Options{MailPDF: true}, reason: "too_large", sends: 2},
		{name: "not read", render: &recRender{}, opts: certificate.Options{MailPDF: true, Artifacts: brokenArtifacts{}}, reason: "read_failed", sends: 2},
		{name: "refused", render: &recRender{}, opts: certificate.Options{MailPDF: true}, withFile: &mail.SendError{Status: http.StatusBadRequest, Code: "attachment.too_large"}, reason: "refused", sends: 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mailer := &fileMailer{withFile: tc.withFile}
			f := mailSetupWith(t, mailer, tc.render, tc.opts)
			if n := f.process(t); n != 2 {
				t.Fatalf("claimed %d", n)
			}
			files, keys := mailer.sends()
			if len(files) != tc.sends {
				t.Fatalf("sends %d", len(files))
			}
			if tc.withFile != nil {
				// Each mail: first with the file (refused), then without,
				// under the same key.
				for i := 0; i < len(files); i += 2 {
					if files[i] == nil || files[i+1] != nil || keys[i] != keys[i+1] {
						t.Fatalf("send %d: %v %v %s %s", i, files[i] != nil, files[i+1] != nil, keys[i], keys[i+1])
					}
				}
			} else {
				for i, got := range files {
					if got != nil {
						t.Fatalf("send %d carried a file", i)
					}
				}
			}
			for serial, state := range f.states(t) {
				if state.Status != certificate.MailSent || state.ErrorCode != "" {
					t.Fatalf("%s: %+v", serial, state)
				}
			}
			if m := metricsOf(t, f.svc); !strings.Contains(m, `core_certificate_mail_pdf_skipped_total{reason="`+tc.reason+`"} 2`) {
				t.Fatalf("metrics\n%s", m)
			}
		})
	}
}

// The file name is the serial's, ASCII alone: never a name, never a
// Turkish letter.
func TestCertificatePDFFilename(t *testing.T) {
	t.Parallel()
	for serial, want := range map[string]string{
		"0A1B2C":       "SKY-LAB-sertifika-0A1B2C.pdf",
		"çğ/..\\İ-x 9": "SKY-LAB-sertifika--x9.pdf",
		"":             "SKY-LAB-sertifika.pdf",
	} {
		if got := certificate.CertificatePDFFilename(serial); got != want {
			t.Fatalf("%q: %q, want %q", serial, got, want)
		}
	}
}
