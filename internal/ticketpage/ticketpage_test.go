package ticketpage_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"errors"
	"image/png"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	qrcode "github.com/skip2/go-qrcode"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/ticketpage"
	"github.com/skylab-kulubu/core-backend/internal/ticketqr"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// countingStore counts the access key lookups that reach the store.
type countingStore struct {
	*ticket.MemoryStore
	lookups atomic.Int32
}

func (s *countingStore) GetByAccessKey(ctx context.Context, key string) (ticket.Ticket, error) {
	s.lookups.Add(1)
	return s.MemoryStore.GetByAccessKey(ctx, key)
}

// fakeRenderer keeps the HTML it was given and answers a small PDF.
type fakeRenderer struct {
	html string
	err  error
}

func (f *fakeRenderer) TaggedPDF(_ context.Context, html string) ([]byte, error) {
	f.html = html
	if f.err != nil {
		return nil, f.err
	}
	return []byte("%PDF-1.7 bilet"), nil
}

type fixture struct {
	t       *testing.T
	store   *countingStore
	events  *event.MemoryStore
	users   *user.MemoryStore
	tickets ticket.Service
	signer  *ticketqr.Signer
	render  *fakeRenderer
	svc     *ticketpage.Service
	ev      event.Event
	now     time.Time
}

var lead = authz.Principal{ID: "lead", Groups: []string{"/UYELER/ARGE/AILAB/LIDERLER"}}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{
		t: t, store: &countingStore{MemoryStore: ticket.NewMemoryStore()}, events: event.NewMemoryStore(), users: user.NewMemoryStore(),
		signer: ticketqr.NewSigner(ticketqr.DeriveKey(key), ticketqr.Config{}), render: &fakeRenderer{},
		now: time.Date(2026, 12, 1, 9, 0, 0, 0, time.UTC),
	}
	az := authz.NewAuthorizer(authz.DefaultPolicy())
	f.tickets = ticket.NewService(f.store, f.events, az, f.users, f.signer)
	start := time.Date(2026, 12, 12, 7, 0, 0, 0, time.UTC)
	end := time.Date(2026, 12, 12, 15, 0, 0, 0, time.UTC)
	f.ev, err = f.events.Create(t.Context(), event.Event{Name: "Yapay Zeka Kampı", Location: "YTÜ Davutpaşa", OwnerTeam: "AILAB", StartDate: &start, EndDate: &end})
	if err != nil {
		t.Fatal(err)
	}
	f.svc = ticketpage.New(f.store, f.events, f.users, f.signer, f.render, nil, ticketpage.Config{
		Page: ticket.PageConfig{Grace: ticket.DefaultPageGrace}, APIOrigin: "https://api.example.test",
	})
	f.svc.Now = func() time.Time { return f.now }
	return f
}

func (f *fixture) guest(email string) ticket.Ticket {
	f.t.Helper()
	applied, err := f.tickets.ApplyGuest(f.t.Context(), lead, f.ev.ID, ticket.GuestInfo{
		FirstName: "Elif", LastName: "Yılmaz", Email: email, PhoneNumber: "+905551112233",
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return applied.Ticket
}

// The JSON names the event, the holder and the code, and nothing else of
// the person: no e-mail, phone, Sky number or ticket type.
func TestPageAnswersOnlyWhatThePageShows(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	g := f.guest("elif@example.com")
	view, err := f.svc.Page(t.Context(), g.AccessKey)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	want := []string{"code", "eventEnd", "eventName", "eventStart", "eventStatus", "holderName", "ownerTeam", "pdfUrl", "qrToken", "qrUrl", "state", "theme", "venue"}
	if !slices.Equal(keys, want) {
		t.Fatalf("keys %v", keys)
	}
	text := string(raw)
	for _, personal := range []string{"elif@example.com", "5551112233", "GUEST", "skyNumber"} {
		if strings.Contains(text, personal) {
			t.Fatalf("answers %q: %s", personal, text)
		}
	}
	if view.HolderName != "Elif Yılmaz" || view.Code != g.Code || view.State != ticketpage.StateActive ||
		view.QRURL != "https://api.example.test/v1/public/tickets/"+g.AccessKey+"/qr.png" ||
		view.PDFURL != "https://api.example.test/v1/public/tickets/"+g.AccessKey+"/pdf" ||
		view.Theme.Key != "kurumsal" || view.EventStatus != "published" {
		t.Fatalf("view %+v", view)
	}
	claims, err := f.signer.Verify(view.QRToken)
	if err != nil || claims.Ticket != g.ID || claims.Event != f.ev.ID || claims.Version != 1 ||
		!claims.ExpiresAt.Equal(f.ev.EndDate.Add(24*time.Hour)) {
		t.Fatalf("qr %+v %v", claims, err)
	}
}

// A member's page names them from core's row.
func TestMemberPage(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	id := uuid.New()
	if _, _, err := user.NewService(f.users).Ensure(t.Context(), id, user.Profile{Email: "ada@example.com", FirstName: "Ada", LastName: "Lovelace"}); err != nil {
		t.Fatal(err)
	}
	created, err := f.tickets.Apply(t.Context(), authz.Principal{ID: id.String()}, f.ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	view, err := f.svc.Page(t.Context(), created.AccessKey)
	if err != nil || view.HolderName != "Ada Lovelace" {
		t.Fatalf("view %+v %v", view, err)
	}
}

func TestPageStates(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	g := f.guest("elif@example.com")
	ctx := t.Context()

	// The Event ended: the page still answers, until a week after it.
	f.now = f.ev.EndDate.Add(6 * 24 * time.Hour)
	if view, err := f.svc.Page(ctx, g.AccessKey); err != nil || view.State != ticketpage.StateEnded || view.QRToken == "" {
		t.Fatalf("ended: %+v %v", view, err)
	}
	f.now = f.ev.EndDate.Add(7 * 24 * time.Hour)
	for name, call := range map[string]func() error{
		"page": func() error { _, err := f.svc.Page(ctx, g.AccessKey); return err },
		"qr":   func() error { _, err := f.svc.QRPNG(ctx, g.AccessKey); return err },
		"pdf":  func() error { _, _, err := f.svc.PDF(ctx, g.AccessKey); return err },
	} {
		if err := call(); !errors.Is(err, ticketpage.ErrGone) {
			t.Fatalf("a week after the end, %s: %v", name, err)
		}
	}

	// Cancelled: the Ticket shows, without QR or PDF; those are gone.
	f.now = time.Date(2026, 12, 1, 9, 0, 0, 0, time.UTC)
	if _, err := f.events.Cancel(ctx, f.ev.ID, nil, "Salon", false); err != nil {
		t.Fatal(err)
	}
	view, err := f.svc.Page(ctx, g.AccessKey)
	if err != nil || view.State != ticketpage.StateEventCancelled || view.QRToken != "" || view.PDFURL != "" || view.QRURL != "" || view.EventStatus != "cancelled" {
		t.Fatalf("cancelled: %+v %v", view, err)
	}
	if _, err := f.svc.QRPNG(ctx, g.AccessKey); !errors.Is(err, ticketpage.ErrGone) {
		t.Fatalf("cancelled qr: %v", err)
	}
	if _, _, err := f.svc.PDF(ctx, g.AccessKey); !errors.Is(err, ticketpage.ErrGone) {
		t.Fatalf("cancelled pdf: %v", err)
	}
	// Undoing the cancellation opens the page again.
	if _, err := f.events.Uncancel(ctx, f.ev.ID, false); err != nil {
		t.Fatal(err)
	}
	if view, err := f.svc.Page(ctx, g.AccessKey); err != nil || view.State != ticketpage.StateActive {
		t.Fatalf("uncancelled: %+v %v", view, err)
	}
}

// Not found: an unknown key, a malformed one (the store is not asked), a
// draft's Ticket, an archived Event's, and a Ticket whose holder was erased.
func TestPageNotFound(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()
	before := f.store.lookups.Load()
	for _, key := range []string{"", "short", "abcdefghijklmnopqrstuvwxyz", "ABCDEFGHIJKLMNOPQRSTUVWXY1", "../../../../etc/passwd"} {
		if _, err := f.svc.Page(ctx, key); !errors.Is(err, ticketpage.ErrNotFound) {
			t.Fatalf("%q: %v", key, err)
		}
	}
	if f.store.lookups.Load() != before {
		t.Fatal("a malformed key reached the store")
	}
	if _, err := f.svc.Page(ctx, "ABCDEFGHIJKLMNOPQRSTUVWXYZ"); !errors.Is(err, ticketpage.ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}

	start := time.Date(2026, 12, 20, 7, 0, 0, 0, time.UTC)
	draft, err := f.events.Create(ctx, event.Event{Name: "Taslak", Location: "YTÜ", OwnerTeam: "AILAB", StartDate: &start, Status: event.StatusDraft})
	if err != nil {
		t.Fatal(err)
	}
	applied, err := f.tickets.ApplyGuest(ctx, lead, draft.ID, ticket.GuestInfo{FirstName: "Ada", LastName: "L", Email: "ada@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Page(ctx, applied.Ticket.AccessKey); !errors.Is(err, ticketpage.ErrNotFound) {
		t.Fatalf("draft: %v", err)
	}

	g := f.guest("elif@example.com")
	erased := g
	erased.GuestFirstName, erased.GuestLastName, erased.GuestEmail = "", "", ""
	if _, err := f.store.Update(ctx, erased); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Page(ctx, g.AccessKey); !errors.Is(err, ticketpage.ErrNotFound) {
		t.Fatalf("erased: %v", err)
	}

	other := f.guest("grace@example.com")
	if err := f.events.Archive(ctx, f.ev.ID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Page(ctx, other.AccessKey); !errors.Is(err, ticketpage.ErrNotFound) {
		t.Fatalf("archived: %v", err)
	}
}

// An Event with no date never ends: its page never closes, and its QR
// lasts a year from the Ticket.
func TestUndatedEventPageStaysOpen(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	undated, err := f.events.Create(t.Context(), event.Event{Name: "Tarihsiz", Location: "YTÜ", OwnerTeam: "AILAB"})
	if err != nil {
		t.Fatal(err)
	}
	applied, err := f.tickets.ApplyGuest(t.Context(), lead, undated.ID, ticket.GuestInfo{FirstName: "Ada", LastName: "L", Email: "ada@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	f.now = time.Now().Add(300 * 24 * time.Hour)
	view, err := f.svc.Page(t.Context(), applied.Ticket.AccessKey)
	if err != nil || view.State != ticketpage.StateActive {
		t.Fatalf("%+v %v", view, err)
	}
	claims, err := f.signer.Verify(view.QRToken)
	stored, _ := f.store.Get(t.Context(), applied.Ticket.ID)
	if err != nil || !claims.ExpiresAt.Equal(stored.CreatedAt.Add(365*24*time.Hour).Truncate(time.Second)) {
		t.Fatalf("expiry %v %v", claims.ExpiresAt, err)
	}
}

// The PNG draws the Ticket QR: module by module it is the token's QR code
// (error correction M, four-module quiet zone), black on white, 512 px.
func TestQRPNGIsTheToken(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	g := f.guest("elif@example.com")
	view, err := f.svc.Page(t.Context(), g.AccessKey)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := f.svc.QRPNG(t.Context(), g.AccessKey)
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != 512 || b.Dy() != 512 {
		t.Fatalf("size %v", b)
	}
	code, err := qrcode.New(view.QRToken, qrcode.Medium)
	if err != nil {
		t.Fatal(err)
	}
	bitmap := code.Bitmap()
	n := len(bitmap)
	for y := 0; y < 4; y++ {
		for x := range bitmap[y] {
			if bitmap[y][x] {
				t.Fatal("no quiet zone")
			}
		}
	}
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			px := img.At(int((float64(x)+0.5)*512/float64(n)), int((float64(y)+0.5)*512/float64(n)))
			r, gr, bl, _ := px.RGBA()
			dark := r+gr+bl < 3*0x8000
			if dark != bitmap[y][x] {
				t.Fatalf("module %d,%d differs", x, y)
			}
		}
	}
}

// The PDF's HTML: Turkish, the holder's name, the code as text, the QR with
// its alternative text, no e-mail or phone; the file is named after the
// Event, not the person.
func TestPDF(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	g := f.guest("elif@example.com")
	name, pdf, err := f.svc.PDF(t.Context(), g.AccessKey)
	if err != nil {
		t.Fatal(err)
	}
	if name != "bilet-yapay-zeka-kampi.pdf" || string(pdf) != "%PDF-1.7 bilet" {
		t.Fatalf("%q %q", name, pdf)
	}
	html := f.render.html
	for _, want := range []string{`<html lang="tr">`, "<title>Yapay Zeka Kampı bileti</title>", "Elif Yılmaz", g.Code, `alt="Bilet QR kodu"`,
		"Kapıda QR", "Okunmazsa bilet kodunu söyle.", "12 Aralık 2026, Cumartesi", "10:00 – 18:00", "YTÜ Davutpaşa", "AILAB", "KOÇAN"} {
		if !strings.Contains(html, want) {
			t.Errorf("HTML has no %q", want)
		}
	}
	for _, personal := range []string{"elif@example.com", "5551112233", "GUEST", "güvenlik özelliği"} {
		if strings.Contains(html, personal) {
			t.Errorf("HTML has %q", personal)
		}
	}

	f.render.err = errors.New("gotenberg down")
	if _, _, err := f.svc.PDF(t.Context(), g.AccessKey); err == nil {
		t.Fatal("a renderer error went unnoticed")
	}
	noRenderer := ticketpage.New(f.store, f.events, f.users, f.signer, nil, nil, ticketpage.Config{})
	if _, _, err := noRenderer.PDF(t.Context(), g.AccessKey); !errors.Is(err, ticketpage.ErrUnavailable) {
		t.Fatalf("no renderer: %v", err)
	}
}

func TestFileNameAndDates(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"Yapay Zeka Kampı":      "bilet-yapay-zeka-kampi.pdf",
		"GECEKODU 2026":         "bilet-gecekodu-2026.pdf",
		"İŞÇİ ÖĞÜT  /  çağ":     "bilet-isci-ogut-cag.pdf",
		"":                      "bilet.pdf",
		"!!!":                   "bilet.pdf",
		`"; rm -rf / ; echo "x`: "bilet-rm-rf-echo-x.pdf",
	} {
		if got := ticketpage.FileName(in); got != want {
			t.Errorf("FileName(%q) = %q, want %q", in, got, want)
		}
	}
	day := func(m time.Month, d, h int) *time.Time {
		v := time.Date(2026, m, d, h, 0, 0, 0, time.UTC)
		return &v
	}
	for _, tc := range []struct {
		start, end *time.Time
		date, time string
	}{
		{nil, nil, "Tarih duyurulacak", "—"},
		{day(12, 12, 7), nil, "12 Aralık 2026, Cumartesi", "10:00"},
		{day(12, 12, 7), day(12, 14, 15), "12–14 Aralık 2026", "10:00 – 18:00"},
		{day(11, 30, 7), day(12, 2, 15), "30 Kasım – 2 Aralık 2026", "10:00 – 18:00"},
	} {
		if got := ticketpage.DateLine(tc.start, tc.end); got != tc.date {
			t.Errorf("DateLine = %q, want %q", got, tc.date)
		}
		if got := ticketpage.TimeLine(tc.start, tc.end); got != tc.time {
			t.Errorf("TimeLine = %q, want %q", got, tc.time)
		}
	}
}
