package mail

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/google/uuid"
)

type certificateMailCall struct {
	key  string
	body map[string]any
}

func certificateMailServer(t *testing.T, statuses ...int) (*httptest.Server, func() []certificateMailCall) {
	t.Helper()
	var mu sync.Mutex
	var calls []certificateMailCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		calls = append(calls, certificateMailCall{key: r.Header.Get("Idempotency-Key"), body: body})
		status := statuses[min(len(calls)-1, len(statuses)-1)]
		mu.Unlock()
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"code":"some.code"}`))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []certificateMailCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]certificateMailCall(nil), calls...)
	}
}

// The queued certificate mail travels under its Idempotency-Key and says
// what SkyNotify answered: taken, refused for good, or worth another try.
func TestCertificateMailReportsSkyNotifysAnswer(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name          string
		status        int
		wantErr       bool
		wantPermanent bool
	}{
		{name: "taken", status: http.StatusCreated},
		{name: "refused body", status: http.StatusBadRequest, wantErr: true, wantPermanent: true},
		{name: "skynotify down", status: http.StatusServiceUnavailable, wantErr: true},
		{name: "rate limited", status: http.StatusTooManyRequests, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv, calls := certificateMailServer(t, tc.status)
			m := &SkyNotify{BaseURL: srv.URL, CertificateTemplateKey: DefaultCertificateTemplateKey, Tokens: StaticToken(testAccessToken), HTTP: srv.Client()}
			err := m.CertificateMail(context.Background(), "core-certificate-mail:1", testRecipient, "Ada Lovelace", map[string]string{"EventName": "ARTLAB"})
			if (err != nil) != tc.wantErr {
				t.Fatalf("err %v", err)
			}
			var refused *SendError
			if tc.wantErr && (!errors.As(err, &refused) || refused.Permanent() != tc.wantPermanent || refused.Status != tc.status) {
				t.Fatalf("err %#v", err)
			}
			got := calls()
			if len(got) != 1 || got[0].key != "core-certificate-mail:1" || got[0].body["template_key"] != DefaultCertificateTemplateKey || got[0].body["recipient_email"] != testRecipient {
				t.Fatalf("calls %+v", got)
			}
		})
	}
}

// A key SkyNotify does not know falls back to the template id under its own
// Idempotency-Key: the body differs from the first one.
func TestCertificateMailFallsBackToTheIDUnderItsOwnKey(t *testing.T) {
	t.Parallel()
	srv, calls := certificateMailServer(t, http.StatusNotFound, http.StatusCreated)
	id := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	m := &SkyNotify{BaseURL: srv.URL, CertificateTemplateKey: DefaultCertificateTemplateKey, CertificateTemplateID: id, Tokens: StaticToken(testAccessToken), HTTP: srv.Client()}
	if err := m.CertificateMail(context.Background(), "k", testRecipient, "Ada", nil); err != nil {
		t.Fatal(err)
	}
	got := calls()
	if len(got) != 2 || got[0].key != "k" || got[1].key != "k:id" || got[1].body["template_id"] != id.String() || got[1].body["template_key"] != nil {
		t.Fatalf("calls %+v", got)
	}
}

// No address, or no template: the kind is off, and the queue says so.
func TestCertificateMailUnconfigured(t *testing.T) {
	t.Parallel()
	if err := (&SkyNotify{CertificateTemplateKey: DefaultCertificateTemplateKey}).CertificateMail(context.Background(), "k", testRecipient, "Ada", nil); !errors.Is(err, ErrTemplateUnconfigured) {
		t.Fatalf("no address: %v", err)
	}
	if err := (&SkyNotify{BaseURL: "http://skynotify.invalid"}).CertificateMail(context.Background(), "k", testRecipient, "Ada", nil); !errors.Is(err, ErrTemplateUnconfigured) {
		t.Fatalf("no template: %v", err)
	}
	var none *SkyNotify
	if err := none.CertificateMail(context.Background(), "k", testRecipient, "Ada", nil); !errors.Is(err, ErrTemplateUnconfigured) {
		t.Fatalf("nil client: %v", err)
	}
}

// The certificate's PDF goes in the body's attachments, by the key and by
// the id fallback alike, under the same Idempotency-Key as without it;
// without one the body has no attachments field.
func TestCertificateMailAttachments(t *testing.T) {
	t.Parallel()
	srv, calls := certificateMailServer(t, http.StatusNotFound, http.StatusCreated, http.StatusCreated)
	id := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	m := &SkyNotify{BaseURL: srv.URL, CertificateTemplateKey: DefaultCertificateTemplateKey, CertificateTemplateID: id, Tokens: StaticToken(testAccessToken), HTTP: srv.Client()}
	pdf := PDFAttachment("SKY-LAB-sertifika-0A1B.pdf", []byte("%PDF-1.4 x"))
	if err := m.CertificateMailAttachments(context.Background(), "k", testRecipient, "Ada", nil, []Attachment{pdf}); err != nil {
		t.Fatal(err)
	}
	if err := m.CertificateMail(context.Background(), "plain", testRecipient, "Ada", nil); err != nil {
		t.Fatal(err)
	}
	got := calls()
	if len(got) != 3 || got[0].key != "k" || got[1].key != "k:id" || got[2].key != "plain" {
		t.Fatalf("calls %+v", got)
	}
	for _, call := range got[:2] {
		files, _ := call.body["attachments"].([]any)
		if len(files) != 1 {
			t.Fatalf("attachments %+v", call.body["attachments"])
		}
		file, _ := files[0].(map[string]any)
		if file["filename"] != pdf.Filename || file["content_type"] != "application/pdf" || file["content_base64"] != pdf.ContentBase64 {
			t.Fatalf("attachment %+v", file)
		}
	}
	if _, ok := got[2].body["attachments"]; ok {
		t.Fatalf("a mail without a file sent attachments: %+v", got[2].body)
	}
}
