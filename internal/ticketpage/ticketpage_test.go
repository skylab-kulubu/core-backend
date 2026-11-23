package ticketpage_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"errors"
	"image"
	"image/jpeg"
	"image/png"
	"net"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	qrcode "github.com/skip2/go-qrcode"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/organizer"
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

// dataURL matches an inline file (a font, an image), whose base64 is not
// text of the ticket.
var dataURL = regexp.MustCompile(`data:[a-z/+0-9-]+;base64,[A-Za-z0-9+/=]+`)

// fakeRenderer keeps the HTML and the page head it was given and answers a
// small PDF.
type fakeRenderer struct {
	html, header string
	err          error
}

func (f *fakeRenderer) TaggedPDF(_ context.Context, html, header string) ([]byte, error) {
	f.html, f.header = html, header
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
	want := []string{"code", "coverColors", "eventEnd", "eventName", "eventStart", "eventStatus", "holderName", "organizerName", "ownerTeam", "pdfUrl", "qrToken", "qrUrl", "state", "theme", "venue"}
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
		view.Theme.Key != "kurumsal" || view.Theme.Accent != ticketpage.SiteAccent || view.EventStatus != "published" ||
		view.CoverImageURL != "" || view.CoverColors == nil || len(view.CoverColors) != 0 ||
		view.DirectionsURL != "" || view.MapsURL != "" || view.OwnerTeam != "AILAB" || view.OrganizerName != "AILAB" {
		t.Fatalf("view %+v", view)
	}
	claims, err := f.signer.Verify(view.QRToken)
	if err != nil || claims.Ticket != g.ID || claims.Event != f.ev.ID || claims.Version != 1 ||
		!claims.ExpiresAt.Equal(f.ev.EndDate.Add(24*time.Hour)) {
		t.Fatalf("qr %+v %v", claims, err)
	}
}

// An Event of the Board (YK), DK or ADMIN is the club's: the page and the
// PDF name the club as its organizer, and the page keeps ownerTeam (Yusuf,
// 2026-11-23).
func TestClubEventNamesTheClub(t *testing.T) {
	t.Parallel()
	for _, team := range []string{"YK", "DK", "ADMIN"} {
		f := newFixture(t)
		f.ev.OwnerTeam = team
		if _, err := f.events.Update(t.Context(), f.ev); err != nil {
			t.Fatal(err)
		}
		applied, err := f.tickets.ApplyGuest(t.Context(), authz.Principal{ID: "admin", Groups: []string{"/ADMIN"}}, f.ev.ID, ticket.GuestInfo{
			FirstName: "Elif", LastName: "Yılmaz", Email: "elif@example.com", PhoneNumber: "+905551112233",
		})
		if err != nil {
			t.Fatal(err)
		}
		g := applied.Ticket
		view, err := f.svc.Page(t.Context(), g.AccessKey)
		if err != nil {
			t.Fatal(err)
		}
		if view.OrganizerName != organizer.ClubName || view.OwnerTeam != team {
			t.Fatalf("%s: organizer %q owner %q", team, view.OrganizerName, view.OwnerTeam)
		}
		if _, _, err := f.svc.PDF(t.Context(), g.AccessKey); err != nil {
			t.Fatal(err)
		}
		if want := "<span>Düzenleyen</span><b>" + organizer.ClubName + "</b>"; !strings.Contains(f.render.html, want) {
			t.Fatalf("%s: the PDF has no %q", team, want)
		}
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

// The PDF's HTML (v5): Turkish, the holder's name, the code as text, the QR
// with its alternative text, the place with its directions QR, the
// footnote; no e-mail or phone, no sentence telling the holder what to do,
// nothing fetched from outside the page. Every page's head names the club,
// the code and the page. The file is named after the Event, not the person.
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
	for _, want := range []string{`<html lang="tr">`, "<title>Etkinlik Giriş Bileti " + g.Code + "</title>", "ETKİNLİK GİRİŞ BİLETİ",
		"Elif Yılmaz", g.Code, `alt="Bilet QR kodu"`, "12 Ara 2026 Cmt", "10:00 – 18:00", "YTÜ Davutpaşa", "AILAB",
		"Etkinlik yeri", "Bu bilet kişiye özeldir. Düzenlenme:",
		"Kulübün kanallarında paylaşılabilir", "skyl.app/kvkk-metni", "@font-face{font-family:'Manrope'", "@page{size:A4;margin:25mm 14mm 14mm}"} {
		if !strings.Contains(html, want) {
			t.Errorf("HTML has no %q", want)
		}
	}
	text := dataURL.ReplaceAllString(html, "data:…")
	for _, unwanted := range []string{"elif@example.com", "5551112233", "GUEST", "güvenlik özelliği",
		"Kapıda", "göster", "Okunmazsa", "söyle", "okut", "dokun", "KOÇAN",
		`src="http`, "url(http", "fonts.googleapis", "<script", g.AccessKey, "/bilet/", "Yol tarifi"} {
		if strings.Contains(text, unwanted) {
			t.Errorf("HTML has %q", unwanted)
		}
	}
	for _, want := range []string{"SKY LAB: Bilgisayar Bilimleri Kulübü", "Bilet kodu", g.Code, `class="pageNumber"`, `class="totalPages"`} {
		if !strings.Contains(f.render.header, want) {
			t.Errorf("page head has no %q", want)
		}
	}

	// Gotenberg unreachable or failing: try again later.
	f.render.err = &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	if _, _, err := f.svc.PDF(t.Context(), g.AccessKey); !errors.Is(err, ticketpage.ErrUnavailable) {
		t.Fatalf("Gotenberg unreachable: %v", err)
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
		start, end                             *time.Time
		firstLabel, first, secondLabel, second string
	}{
		{nil, nil, "Tarih", "Duyurulacak", "Saat", "—"},
		{day(12, 12, 7), nil, "Tarih", "12 Ara 2026 Cmt", "Saat", "10:00"},
		{day(12, 12, 7), day(12, 12, 15), "Tarih", "12 Ara 2026 Cmt", "Saat", "10:00 – 18:00"},
		{day(12, 12, 6), day(12, 14, 14), "Başlangıç", "12 Ara 2026 Cmt, 09:00", "Bitiş", "14 Ara 2026 Pzt, 17:00"},
		{day(11, 30, 7), day(12, 2, 15), "Başlangıç", "30 Kas 2026 Pzt, 10:00", "Bitiş", "2 Ara 2026 Çar, 18:00"},
	} {
		firstLabel, first, secondLabel, second := ticketpage.When(tc.start, tc.end)
		if firstLabel != tc.firstLabel || first != tc.first || secondLabel != tc.secondLabel || second != tc.second {
			t.Errorf("When = %q %q %q %q", firstLabel, first, secondLabel, second)
		}
	}
	if got := ticketpage.SessionTime(day(12, 12, 6), day(12, 12, 7)); got != "09:00–10:00" {
		t.Errorf("SessionTime = %q", got)
	}
	if got := ticketpage.SessionTime(day(12, 12, 6), nil); got != "09:00" {
		t.Errorf("SessionTime without an end = %q", got)
	}
	if got := ticketpage.SessionTime(nil, nil); got != "" {
		t.Errorf("SessionTime without a start = %q", got)
	}
	if got := ticketpage.DayLine(*day(12, 13, 9)); got != "13 Aralık Pazar" {
		t.Errorf("DayLine = %q", got)
	}
}

// fakeImages answers one cover's page size.
type fakeImages struct {
	id    uuid.UUID
	data  []byte
	asked []string
}

func (f *fakeImages) Image(_ context.Context, id uuid.UUID, size string) ([]byte, string, bool) {
	f.asked = append(f.asked, size)
	if id != f.id {
		return nil, "", false
	}
	return f.data, "image/jpeg", true
}

// The PDF reads the programme from the Event's current days and Sessions,
// in time order, without the cancelled ones; its band is cut from the
// cover's page size; the place has its address and directions to its point.
func TestPDFReadsTheEvent(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()
	lat, lng := 41.025376, 28.889648
	cover := uuid.New()
	ev := f.ev
	ev.LocationLat, ev.LocationLng, ev.LocationAddress = &lat, &lng, "Davutpaşa Cd. No:127"
	ev.CoverImageID, ev.Description = &cover, "Uygulamalı bir kamp."
	if _, err := f.events.Update(ctx, ev); err != nil {
		t.Fatal(err)
	}
	at := func(h, m int) *time.Time {
		v := time.Date(2026, 12, 12, h, m, 0, 0, time.UTC)
		return &v
	}
	day, err := f.events.CreateDay(ctx, event.Day{EventID: ev.ID, Name: "Cumartesi", StartDate: at(6, 30)})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []event.Session{
		{EventDayID: day.ID, Title: "Kapanış", StartTime: at(14, 0), EndTime: at(15, 0)},
		{EventDayID: day.ID, Title: "Açılış", SpeakerName: "Ada", StartTime: at(6, 30), EndTime: at(7, 0)},
		{EventDayID: day.ID, Title: "İptal edilen", StartTime: at(9, 0), Cancelled: true},
	} {
		if _, err := f.events.CreateSession(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	photo := image.NewRGBA(image.Rect(0, 0, 1200, 800))
	var src bytes.Buffer
	if err := jpeg.Encode(&src, photo, nil); err != nil {
		t.Fatal(err)
	}
	images := &fakeImages{id: cover, data: src.Bytes()}
	svc := ticketpage.New(f.store, f.events, f.users, f.signer, f.render, images, ticketpage.Config{
		Page: ticket.PageConfig{Grace: ticket.DefaultPageGrace, URL: "https://yildizskylab.com/bilet/{key}"},
	})
	svc.Now = func() time.Time { return f.now }
	g := f.guest("elif@example.com")
	if _, _, err := svc.PDF(ctx, g.AccessKey); err != nil {
		t.Fatal(err)
	}
	html := f.render.html
	open, closing := strings.Index(html, ">Açılış<"), strings.Index(html, ">Kapanış<")
	if open < 0 || closing < open || strings.Contains(html, "İptal edilen") {
		t.Errorf("programme order %d %d, or a cancelled Session drawn", open, closing)
	}
	for _, want := range []string{"09:30–10:00", `<td class="kisi">Ada</td>`, "Davutpaşa Cd. No:127", "Uygulamalı bir kamp.",
		"background-image:url(", `alt="Yol tarifi QR kodu"`} {
		if !strings.Contains(html, want) {
			t.Errorf("HTML has no %q", want)
		}
	}
	if strings.Contains(html, g.AccessKey) {
		t.Error("the PDF carries the ticket page's key")
	}
	if len(images.asked) != 1 || images.asked[0] != "page" {
		t.Errorf("asked %v", images.asked)
	}
	view, err := svc.Page(ctx, g.AccessKey)
	if err != nil || view.VenueAddress != "Davutpaşa Cd. No:127" || view.VenueLat == nil || *view.VenueLat != lat ||
		view.DirectionsURL != "https://www.google.com/maps/dir/?api=1&destination=41.025376%2C28.889648" ||
		view.MapsURL != "https://www.google.com/maps/search/?api=1&query=41.025376%2C28.889648" {
		t.Fatalf("view %+v %v", view, err)
	}
	// A second PDF uses the band it cut.
	if _, _, err := svc.PDF(ctx, g.AccessKey); err != nil || len(images.asked) != 1 {
		t.Fatalf("second PDF asked %v %v", images.asked, err)
	}
}

// The place's map: the PDF embeds it with its credit, and the JSON gives
// its address and the credit; without one neither has a map.
func TestMapInPDFAndPage(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()
	mapID := uuid.New()
	ev := f.ev
	ev.MapImageID, ev.MapImageURL = &mapID, "images/harita.jpg"
	if _, err := f.events.Update(ctx, ev); err != nil {
		t.Fatal(err)
	}
	picture := []byte{0xff, 0xd8, 0xff, 0xe0, 'h', 'a', 'r', 'i', 't', 'a'}
	images := &fakeImages{id: mapID, data: picture}
	svc := ticketpage.New(f.store, f.events, f.users, f.signer, f.render, images, ticketpage.Config{Page: ticket.PageConfig{Grace: ticket.DefaultPageGrace}})
	svc.Now = func() time.Time { return f.now }
	g := f.guest("elif@example.com")
	if _, _, err := svc.PDF(ctx, g.AccessKey); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.render.html, `<img class="harita" src="data:image/jpeg;base64,`) || !strings.Contains(f.render.html, `<p class="atif">© OpenStreetMap contributors · openstreetmap.org/copyright</p>`) {
		t.Error("the PDF has no map, or no credit")
	}
	if len(images.asked) != 1 || images.asked[0] != "" {
		t.Errorf("asked %v", images.asked)
	}
	view, err := svc.Page(ctx, g.AccessKey)
	if err != nil || !strings.HasSuffix(view.MapImageURL, "/images/harita.jpg") || view.MapAttribution != "© OpenStreetMap contributors" || view.MapAttributionURL != "https://www.openstreetmap.org/copyright" {
		t.Fatalf("view %q %q %v", view.MapImageURL, view.MapAttribution, err)
	}
	ev.MapImageID, ev.MapImageURL = nil, ""
	if _, err := f.events.Update(ctx, ev); err != nil {
		t.Fatal(err)
	}
	if view, err := svc.Page(ctx, g.AccessKey); err != nil || view.MapImageURL != "" || view.MapAttribution != "" || view.MapAttributionURL != "" {
		t.Fatalf("without a map %+v %v", view, err)
	}
}

// The page's cover and accent: the cover's public address and colours, the
// accent its first colour, else the club's purple.
func TestPageCoverAndAccent(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()
	cover := uuid.New()
	ev := f.ev
	ev.CoverImageID, ev.CoverImageURL, ev.CoverColors = &cover, "images/kapak.jpg", []string{"bozuk", "#0f766e", "#111111"}
	if _, err := f.events.Update(ctx, ev); err != nil {
		t.Fatal(err)
	}
	g := f.guest("elif@example.com")
	view, err := f.svc.Page(ctx, g.AccessKey)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(view.CoverImageURL, "/images/kapak.jpg") || len(view.CoverColors) != 3 || view.Theme.Accent != "#0F766E" {
		t.Fatalf("cover %q %v, accent %q", view.CoverImageURL, view.CoverColors, view.Theme.Accent)
	}
	for _, tc := range []struct {
		colors []string
		want   string
	}{
		{nil, ticketpage.SiteAccent}, {[]string{"red"}, ticketpage.SiteAccent}, {[]string{"#abcdef"}, "#ABCDEF"},
	} {
		if got := ticketpage.Accent(event.Event{CoverColors: tc.colors}); got != tc.want {
			t.Errorf("%v: %q", tc.colors, got)
		}
	}
}
