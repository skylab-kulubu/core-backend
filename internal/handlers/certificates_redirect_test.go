package handlers

import (
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
)

func TestCertificateHTTP_PublicPageRedirectsToClubSite(t *testing.T) {
	t.Setenv("CERTIFICATE_PUBLIC_PAGE_ORIGIN", "https://yildizskylab.com/sertifika/")
	app := fiber.New()
	app.Get("/c/:serial", NewCertificateHandler(nil).PublicPage)

	const serial = "75E614C7A33C08CB5C04804D6C24F9E7"
	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/c/"+serial, nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusFound {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if got, want := resp.Header.Get(fiber.HeaderLocation), "https://yildizskylab.com/sertifika/"+serial; got != want {
		t.Fatalf("location %q want %q", got, want)
	}
}
