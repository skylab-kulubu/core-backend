package handlers

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/ticketpage"
	"github.com/skylab-kulubu/core-backend/internal/ticketqr"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

type pdfStub struct{}

func (pdfStub) TaggedPDF(_ context.Context, _ string) ([]byte, error) {
	return []byte("%PDF-1.7 bilet"), nil
}

func ticketPageApp(t *testing.T) (*fiber.App, ticket.Ticket, *event.MemoryStore, event.Event) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer := ticketqr.NewSigner(ticketqr.DeriveKey(key), ticketqr.Config{})
	store, events, users := ticket.NewMemoryStore(), event.NewMemoryStore(), user.NewMemoryStore()
	svc := ticket.NewService(store, events, authz.NewAuthorizer(authz.DefaultPolicy()), users, signer)
	end := time.Now().Add(72 * time.Hour)
	ev, err := events.Create(t.Context(), event.Event{Name: "Yapay Zeka Kampı", Location: "YTÜ", OwnerTeam: "AILAB", EndDate: &end})
	if err != nil {
		t.Fatal(err)
	}
	applied, err := svc.ApplyGuest(t.Context(), authz.Principal{ID: "lead", Groups: []string{"/UYELER/ARGE/AILAB/LIDERLER"}}, ev.ID,
		ticket.GuestInfo{FirstName: "Elif", LastName: "Yılmaz", Email: "elif@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	page := NewTicketPageHandler(ticketpage.New(store, events, users, signer, pdfStub{}, nil, ticketpage.Config{Page: ticket.PageConfig{Grace: ticket.DefaultPageGrace}}))
	app := fiber.New(fiber.Config{ErrorHandler: ErrorHandler})
	app.Get("/v1/public/tickets/:key", page.Page)
	app.Get("/v1/public/tickets/:key/qr.png", page.QR)
	app.Get("/v1/public/tickets/:key/pdf", TicketPDFLimit(), page.PDF)
	return app, applied.Ticket, events, ev
}

func getTicketPage(t *testing.T, app *fiber.App, path string) (int, string, map[string]string) {
	t.Helper()
	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, path, nil))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	headers := map[string]string{}
	for _, name := range []string{"Cache-Control", "Referrer-Policy", "X-Robots-Tag", "Content-Type", "Content-Disposition"} {
		headers[name] = resp.Header.Get(name)
	}
	return resp.StatusCode, string(body), headers
}

// The three routes answer without a sign-in, keep the key out of caches,
// referrers and indexes, and the PDF is an attachment named after the Event.
func TestTicketPageRoutes(t *testing.T) {
	t.Parallel()
	app, tk, events, ev := ticketPageApp(t)
	base := "/v1/public/tickets/" + tk.AccessKey
	for path, contentType := range map[string]string{base: "application/json", base + "/qr.png": "image/png", base + "/pdf": "application/pdf"} {
		status, body, headers := getTicketPage(t, app, path)
		if status != fiber.StatusOK || !strings.HasPrefix(headers["Content-Type"], contentType) {
			t.Fatalf("%s: %d %s %.200s", path, status, headers["Content-Type"], body)
		}
		if headers["Cache-Control"] != "no-store" || headers["Referrer-Policy"] != "no-referrer" || headers["X-Robots-Tag"] != "noindex, nofollow" {
			t.Fatalf("%s headers %v", path, headers)
		}
	}
	_, _, headers := getTicketPage(t, app, base+"/pdf")
	if headers["Content-Disposition"] != `attachment; filename="bilet-yapay-zeka-kampi.pdf"` {
		t.Fatalf("disposition %q", headers["Content-Disposition"])
	}
	status, body, _ := getTicketPage(t, app, base)
	var view map[string]any
	if err := json.Unmarshal([]byte(body), &view); err != nil || status != fiber.StatusOK || view["holderName"] != "Elif Yılmaz" {
		t.Fatalf("%d %s", status, body)
	}

	// Unknown and malformed keys: 404 with nothing about anyone.
	for _, key := range []string{"ABCDEFGHIJKLMNOPQRSTUVWXYZ", "nope"} {
		if status, body, _ := getTicketPage(t, app, "/v1/public/tickets/"+key); status != fiber.StatusNotFound || strings.Contains(body, "Elif") {
			t.Fatalf("%s: %d %s", key, status, body)
		}
	}

	// Cancelled: the QR and the PDF are gone (410), the page says so.
	if _, err := events.Cancel(t.Context(), ev.ID, nil, "", false); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{base + "/qr.png", base + "/pdf"} {
		if status, body, _ := getTicketPage(t, app, path); status != fiber.StatusGone || !strings.Contains(body, "ticket_page_closed") || strings.Contains(body, "Elif") {
			t.Fatalf("%s: %d %s", path, status, body)
		}
	}
}

// Five PDFs a minute for one key; another key has its own budget.
func TestTicketPDFLimitPerKey(t *testing.T) {
	t.Parallel()
	app, tk, _, _ := ticketPageApp(t)
	path := "/v1/public/tickets/" + tk.AccessKey + "/pdf"
	for i := range TicketPDFsPerKeyMinute {
		if status, _, _ := getTicketPage(t, app, path); status != fiber.StatusOK {
			t.Fatalf("pdf %d: %d", i+1, status)
		}
	}
	status, body, headers := getTicketPage(t, app, path)
	if status != fiber.StatusTooManyRequests || !strings.Contains(body, "ticket_pdf_rate_limited") || headers["Cache-Control"] != "no-store" {
		t.Fatalf("sixth: %d %s", status, body)
	}
	if status, _, _ := getTicketPage(t, app, "/v1/public/tickets/ABCDEFGHIJKLMNOPQRSTUVWXYZ/pdf"); status != fiber.StatusNotFound {
		t.Fatalf("another key: %d", status)
	}
}

// The holder's own list carries the ticket page and the Ticket QR; the
// panel's read of the same Ticket carries neither.
func TestMineCarriesTheTicketPageAndQR(t *testing.T) {
	t.Parallel()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	signer := ticketqr.NewSigner(ticketqr.DeriveKey(key), ticketqr.Config{})
	store, events, users := ticket.NewMemoryStore(), event.NewMemoryStore(), user.NewMemoryStore()
	for _, pageURL := range []string{"https://yildizskylab.com/bilet/{key}", ""} {
		svc := ticket.NewService(store, events, authz.NewAuthorizer(authz.DefaultPolicy()), users, signer, ticket.PageConfig{URL: pageURL, Grace: ticket.DefaultPageGrace})
		end := time.Now().Add(72 * time.Hour)
		ev, err := events.Create(t.Context(), event.Event{Name: "Kamp", Location: "YTÜ", OwnerTeam: "AILAB", EndDate: &end})
		if err != nil {
			t.Fatal(err)
		}
		holder := uuid.New()
		if _, _, err := user.NewService(users).Ensure(t.Context(), holder, user.Profile{Email: holder.String() + "@example.com", FirstName: "Ada", LastName: "L"}); err != nil {
			t.Fatal(err)
		}
		p := authz.Principal{ID: holder.String()}
		created, err := svc.Apply(t.Context(), p, ev.ID)
		if err != nil {
			t.Fatal(err)
		}
		mine, err := svc.Mine(t.Context(), p)
		if err != nil || len(mine) != 1 {
			t.Fatalf("mine %v %v", mine, err)
		}
		wantURL := ""
		if pageURL != "" {
			wantURL = "https://yildizskylab.com/bilet/" + created.AccessKey
		}
		if mine[0].TicketURL != wantURL || mine[0].QRToken == "" || mine[0].Code != created.Code {
			t.Fatalf("page %q: mine %+v", pageURL, mine[0])
		}
		if _, err := signer.Verify(mine[0].QRToken); err != nil {
			t.Fatal(err)
		}
		panel, err := svc.Get(t.Context(), authz.Principal{ID: "yk", Groups: []string{"/UYELER/YK"}}, created.ID)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(panel)
		if strings.Contains(string(raw), "ticketUrl") || strings.Contains(string(raw), "qrToken") || strings.Contains(string(raw), created.AccessKey) {
			t.Fatalf("the panel's read: %s", raw)
		}
	}
}
