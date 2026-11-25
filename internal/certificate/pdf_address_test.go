package certificate_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/certificate"
)

// pdfURLs issues the ARTLAB fixture's certificate with a service built from
// options and answers its pdfUrl in Verify and in Mine.
func pdfURLs(t *testing.T, opts certificate.Options) (serial, verified, mine string) {
	t.Helper()
	ctx := context.Background()
	events, tickets, users, ticketSvc, _, render, mailer, _, tk, sessions, leader := artlabSetup(t, 0.75)
	opts.LegacyImmediate = true
	certs := certificate.NewServiceWithOptions(certificate.NewMemoryStore(), tickets, events, users,
		authz.NewAuthorizer(authz.DefaultPolicy()), render, mailer, opts)
	checkInN(t, ticketSvc, leader, tk.ID, sessions, 6)
	issued, err := certs.RecomputeTicket(ctx, tk.ID)
	if err != nil || issued == nil {
		t.Fatalf("issue: %v %+v", err, issued)
	}
	got, err := certs.Verify(ctx, issued.Serial)
	if err != nil {
		t.Fatal(err)
	}
	owner := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	listed, err := certs.Mine(ctx, authz.Principal{ID: owner.String()})
	if err != nil || len(listed) != 1 {
		t.Fatalf("mine: %v %+v", err, listed)
	}
	return issued.Serial, got.PDFURL, listed[0].PDFURL
}

func TestPDFURLOnTheCertificatePage(t *testing.T) {
	t.Parallel()
	serial, verified, mine := pdfURLs(t, certificate.Options{
		PublicAPIOrigin: "https://api.example.test", VerifyOrigin: "https://skyl.app/c",
		PublicPageOrigin: "https://yildizskylab.com/sertifika/",
	})
	want := "https://yildizskylab.com/sertifika/" + serial + ".pdf"
	if verified != want || mine != want {
		t.Fatalf("pdfUrl verify %q mine %q, want %q", verified, mine, want)
	}
}

func TestPDFURLStaysOnCoreWithoutThePageOrigin(t *testing.T) {
	t.Parallel()
	serial, verified, mine := pdfURLs(t, certificate.Options{
		PublicAPIOrigin: "https://api.example.test", VerifyOrigin: "https://skyl.app/c",
	})
	want := "https://api.example.test/v1/certificates/verify/" + serial + "/pdf"
	if verified != want || mine != want {
		t.Fatalf("pdfUrl verify %q mine %q, want %q", verified, mine, want)
	}
}

func TestPublicPageOrigin(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]string{
		"":                                   "",
		"   ":                                "",
		"https://yildizskylab.com/sertifika": "https://yildizskylab.com/sertifika",
		" https://sandbox.yildizskylab.com/sertifika/ ": "https://sandbox.yildizskylab.com/sertifika",
		"http://localhost:3000/sertifika":               "http://localhost:3000/sertifika",
	} {
		got, err := certificate.PublicPageOrigin(raw)
		if err != nil || got != want {
			t.Errorf("PublicPageOrigin(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	for _, raw := range []string{
		"yildizskylab.com/sertifika",
		"ftp://yildizskylab.com/sertifika",
		"https:///sertifika",
		"https://yildizskylab.com/sertifika?x=1",
		"https://yildizskylab.com/sertifika#x",
		"https://user@yildizskylab.com/sertifika",
	} {
		if got, err := certificate.PublicPageOrigin(raw); err == nil {
			t.Errorf("PublicPageOrigin(%q) = %q, want an error", raw, got)
		}
	}
}
