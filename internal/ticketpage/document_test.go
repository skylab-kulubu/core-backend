package ticketpage_test

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"math/rand/v2"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/skylab-kulubu/core-backend/internal/ticketpage"
)

// The PDF v5 rules (Yusuf, 2026-11-22; docs/ticket-page.md): ticket,
// programme, description, place, footnote, in that order; days side by
// side, at most two to a row; one day alone takes the whole width.

func at(s string) *time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return &t
}

func session(start, end, title, speaker string) ticketpage.ProgramSession {
	return ticketpage.ProgramSession{Start: at(start), End: at(end), Title: title, Speaker: speaker}
}

func baseDocument() ticketpage.Document {
	return ticketpage.Document{
		EventName: "Yapay Zeka Kampı", Start: at("2026-12-12T09:30:00+03:00"), End: at("2026-12-14T17:00:00+03:00"),
		Venue: "YTÜ Davutpaşa, EEF Konferans Salonu", VenueAddress: "Davutpaşa Cd. No:127, Esenler/İstanbul",
		DirectionsURL: "https://www.google.com/maps/dir/?api=1&destination=41.025376%2C28.889648",
		OwnerTeam:     "AILAB", HolderName: "Elif Yılmaz", Code: "K7M2-QX9F", QRToken: "eyJhbGciOiJFUzI1NiJ9.e30.c2ln",
		Description: "Uygulamalı bir kamp.",
		IssuedAt:    time.Date(2026, 11, 22, 12, 40, 0, 0, time.UTC),
	}
}

func programDays(n, sessions int) []ticketpage.ProgramDay {
	out := make([]ticketpage.ProgramDay, n)
	for d := range n {
		day := time.Date(2026, 12, 12+d, 7, 0, 0, 0, time.UTC)
		out[d].Date = &day
		for s := range sessions {
			start := day.Add(time.Duration(s) * time.Hour)
			end := start.Add(50 * time.Minute)
			out[d].Sessions = append(out[d].Sessions, ticketpage.ProgramSession{
				Start: &start, End: &end, Title: "Oturum " + string(rune('A'+s)), Speaker: "Konuşmacı",
			})
		}
	}
	return out
}

func render(t *testing.T, d ticketpage.Document) (string, string) {
	t.Helper()
	html, header, err := ticketpage.RenderHTML(d)
	if err != nil {
		t.Fatal(err)
	}
	return html, header
}

var columns = regexp.MustCompile(`--sutun:(\d+)`)

// Days stand side by side, two to a row at most: a third day starts a new
// row (the grid's two columns take it), never a third column. One day takes
// the whole width and needs no heading.
func TestProgramHasAtMostTwoColumns(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		days, sessions, columns int
		headings                int
		single                  bool
	}{
		{1, 1, 1, 0, true},
		{1, 5, 1, 0, false},
		{2, 6, 2, 2, false},
		{3, 5, 2, 3, false},
		{4, 3, 2, 4, false},
		{5, 2, 2, 5, false},
	} {
		d := baseDocument()
		d.Days = programDays(tc.days, tc.sessions)
		html, _ := render(t, d)
		m := columns.FindStringSubmatch(html)
		if m == nil || m[1] != string(rune('0'+tc.columns)) {
			t.Errorf("%d days: columns %v", tc.days, m)
		}
		if got := strings.Count(html, "<h3>"); got != tc.headings {
			t.Errorf("%d days: %d headings", tc.days, got)
		}
		if got := strings.Contains(html, `<table class="tek">`); got != tc.single {
			t.Errorf("%d days of %d: single %v", tc.days, tc.sessions, got)
		}
		if got := strings.Count(html, `<td class="saat">`); got != tc.days*tc.sessions {
			t.Errorf("%d days: %d rows", tc.days, got)
		}
		if !strings.Contains(html, "grid-template-columns:repeat(var(--sutun),1fr)") {
			t.Error("the days are not a grid of --sutun columns")
		}
	}
	d := baseDocument()
	d.Days = programDays(3, 1)
	html, _ := render(t, d)
	for i, want := range []string{"1. gün <span>· 12 Aralık Cumartesi</span>", "2. gün <span>· 13 Aralık Pazar</span>", "3. gün <span>· 14 Aralık Pazartesi</span>"} {
		if !strings.Contains(html, want) {
			t.Errorf("day %d: no %q", i+1, want)
		}
	}
	if strings.Index(html, "1. gün") > strings.Index(html, "2. gün") || strings.Index(html, "2. gün") > strings.Index(html, "3. gün") {
		t.Error("days out of order")
	}
}

// A row is its hours, its title (whole, wrapped when long) and its speaker;
// a day with no Session and an Event with no programme draw none.
func TestProgramRows(t *testing.T) {
	t.Parallel()
	d := baseDocument()
	long := strings.Repeat("Çok uzun bir oturum adı ", 8)
	d.Days = []ticketpage.ProgramDay{
		{Date: at("2026-12-12T00:00:00+03:00"), Sessions: []ticketpage.ProgramSession{
			session("2026-12-12T09:30:00+03:00", "2026-12-12T10:00:00+03:00", "Açılış", ""),
			session("2026-12-12T10:00:00+03:00", "2026-12-12T11:00:00+03:00", long, "Zeynep Arslan"),
		}},
		{},
		{Date: at("2026-12-13T00:00:00+03:00"), Sessions: []ticketpage.ProgramSession{
			session("2026-12-13T10:00:00+03:00", "2026-12-13T11:00:00+03:00", "Büyük dil modelleri", "Selin Aydın"),
		}},
	}
	html, _ := render(t, d)
	for _, want := range []string{
		`<td class="saat">09:30–10:00</td><td class="otr">Açılış</td><td class="kisi"></td>`,
		`<td class="otr">` + long + `</td><td class="kisi">Zeynep Arslan</td>`,
		"2. gün <span>· 13 Aralık Pazar</span>",
		"tr{break-inside:avoid}", "break-after:avoid",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("no %q", want)
		}
	}
	if strings.Contains(html, "3. gün") || strings.Contains(html, "…") {
		t.Error("an empty day was drawn, or a title cut")
	}
	d.Days = nil
	html, _ = render(t, d)
	if strings.Contains(html, "<h2>Program</h2>") {
		t.Error("a programme without Sessions")
	}
}

// The sections come in the decided order, the place last before the
// footnote; the description and the address are left out while empty, the
// place never.
func TestSectionsInOrder(t *testing.T) {
	t.Parallel()
	d := baseDocument()
	d.Days = programDays(2, 2)
	html, _ := render(t, d)
	order := []string{"ETKİNLİK GİRİŞ BİLETİ", `class="izgara"`, "<h2>Program</h2>", "<h2>Etkinlik hakkında</h2>", "<h2>Etkinlik yeri</h2>", `<footer class="dip">`}
	last := -1
	for _, mark := range order {
		i := strings.Index(html, mark)
		if i < 0 || i < last {
			t.Fatalf("%q at %d (after %d)", mark, i, last)
		}
		last = i
	}
	if !strings.Contains(html, ".yer{break-inside:avoid}") || !strings.Contains(html, ".izgara{display:grid;grid-template-columns:1fr 1fr 54mm;") {
		t.Error("the place may split, or the ticket's grid changed")
	}
	d.Description, d.VenueAddress, d.Days = " ", "", nil
	html, _ = render(t, d)
	if strings.Contains(html, "Etkinlik hakkında") || strings.Contains(html, `class="adres"`) || !strings.Contains(html, "<h2>Etkinlik yeri</h2>") {
		t.Error("empty sections drawn, or the place left out")
	}
}

// The fields: an Event within a day shows its date and hours, a longer one
// its start and end, an undated one says so; the holder and the organizer.
func TestTicketFields(t *testing.T) {
	t.Parallel()
	d := baseDocument()
	html, _ := render(t, d)
	for _, want := range []string{"<span>Başlangıç</span><b class=\"buyuk\">12 Ara 2026 Cmt, 09:30</b>", "<span>Bitiş</span><b class=\"buyuk\">14 Ara 2026 Pzt, 17:00</b>",
		"<span>Katılımcı</span><b>Elif Yılmaz</b>", "<span>Düzenleyen</span><b>AILAB</b>", "<span>Etkinlik</span><b class=\"buyuk\">Yapay Zeka Kampı</b>",
		"<p>Bu bilet kişiye özeldir. Düzenlenme: 22.11.2026 15:40</p>",
		"<p>Etkinlikte fotoğraf ve video çekilir, Kulübün kanallarında paylaşılabilir. Kişisel veriler KVKK Metni&#39;ne göre işlenir: skyl.app/kvkk-metni</p>"} {
		if !strings.Contains(html, want) {
			t.Errorf("no %q", want)
		}
	}
	d.Start, d.End = at("2026-12-12T14:00:00+03:00"), at("2026-12-12T16:00:00+03:00")
	d.OwnerTeam = ""
	html, _ = render(t, d)
	for _, want := range []string{"<span>Tarih</span><b class=\"buyuk\">12 Ara 2026 Cmt</b>", "<span>Saat</span><b class=\"buyuk\">14:00 – 16:00</b>", "<span>Düzenleyen</span><b>SKY LAB</b>"} {
		if !strings.Contains(html, want) {
			t.Errorf("no %q", want)
		}
	}
	if strings.Contains(html, "Doğrulama") || strings.Contains(html, "/bilet/") {
		t.Error("the footnote names the ticket page")
	}
}

// The place: its map with the data's credit under it when there is one, and
// the directions QR either way.
func TestPlaceWithAndWithoutMap(t *testing.T) {
	t.Parallel()
	d := baseDocument()
	html, _ := render(t, d)
	if strings.Contains(html, `class="harita"`) || !strings.Contains(html, `alt="Yol tarifi QR kodu"`) {
		t.Error("without a map: a map drawn, or no directions")
	}
	d.Map, d.MapType, d.MapAttribution = []byte{0xff, 0xd8, 0xff}, "image/jpeg", "© OpenStreetMap katkıda bulunanlar"
	html, _ = render(t, d)
	if !strings.Contains(html, `<img class="harita" src="data:image/jpeg;base64,`) || !strings.Contains(html, `alt="Etkinlik yeri haritası"`) ||
		!strings.Contains(html, `<p class="atif">© OpenStreetMap katkıda bulunanlar</p>`) {
		t.Error("the map or its credit is missing")
	}
	d.MapType = "image/svg+xml"
	html, _ = render(t, d)
	if strings.Contains(html, `class="harita"`) {
		t.Error("a map of a type the PDF does not take")
	}
}

// The cover band: the cover's picture cut to the band, else its colour,
// else the theme's.
func TestCoverBand(t *testing.T) {
	t.Parallel()
	photo := image.NewRGBA(image.Rect(0, 0, 1200, 800))
	r := rand.New(rand.NewPCG(1, 2))
	for y := range 800 {
		for x := range 1200 {
			photo.Set(x, y, color.RGBA{uint8(x / 5), uint8(y / 4), uint8(100 + r.IntN(40)), 255})
		}
	}
	var src bytes.Buffer
	if err := jpeg.Encode(&src, photo, &jpeg.Options{Quality: 85}); err != nil {
		t.Fatal(err)
	}
	band, ok := ticketpage.CoverBand(src.Bytes())
	if !ok {
		t.Fatal("no band")
	}
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(band))
	if err != nil || cfg.Width != 1240 || cfg.Height != 164 {
		t.Fatalf("band %+v %v", cfg, err)
	}
	if len(band) > 60<<10 {
		t.Fatalf("band of %d bytes", len(band))
	}
	if _, ok := ticketpage.CoverBand([]byte("bir resim değil")); ok {
		t.Fatal("a band of no picture")
	}
	d := baseDocument()
	d.BandJPEG = band
	if html, _ := render(t, d); !strings.Contains(html, `<div class="kapak" style="background-image:url(`) || !strings.Contains(html, "data:image/jpeg;base64,") {
		t.Error("band not drawn")
	}
	d.BandJPEG, d.BandColor = nil, "#0f766e"
	if html, _ := render(t, d); !strings.Contains(html, `style="background-color:#0f766e"`) {
		t.Error("cover colour not used")
	}
	d.BandColor = "red;}body{display:none"
	if html, _ := render(t, d); !strings.Contains(html, `style="background-color:#7C3AED"`) {
		t.Error("a colour that is not one was used")
	}
}

// The page head of every page: the mark, the club, the code, the page.
func TestPageHead(t *testing.T) {
	t.Parallel()
	html, header := render(t, baseDocument())
	if strings.Contains(html+header, "ZgotmplZ") {
		t.Fatal("a value the template refused")
	}
	for _, want := range []string{"<b>SKY LAB: Bilgisayar Bilimleri Kulübü</b>", "<b>K7M2-QX9F</b>", `<span class="pageNumber"></span>/<span class="totalPages"></span>`,
		"border-bottom:1pt solid #7C3AED", `viewBox="0 0 586.43 553.91"`, "font-size:8.5pt", "font-family:'Noto Sans','Liberation Sans'"} {
		if !strings.Contains(header, want) {
			t.Errorf("head has no %q", want)
		}
	}
}

// What the renderer receives stays under the mail attachment's 512 KB even
// for a long ticket: a three-day programme, a long description, the cover
// band and a map. Chromium embeds the images as they are and the fonts'
// used glyphs only, so the PDF is smaller than this (measured with
// Chromium: docs/ticket-page.md).
func TestDocumentSize(t *testing.T) {
	t.Parallel()
	d := baseDocument()
	d.Days = programDays(3, 8)
	d.Description = strings.Repeat("Yapay Zeka Kampı, makine öğrenmesine yeni başlayanlar için uygulamalı bir kamptır. ", 40)
	photo := image.NewRGBA(image.Rect(0, 0, 1200, 800))
	for y := range 800 {
		for x := range 1200 {
			photo.Set(x, y, color.RGBA{uint8(x / 5), uint8(y / 4), 120, 255})
		}
	}
	var src bytes.Buffer
	if err := jpeg.Encode(&src, photo, &jpeg.Options{Quality: 85}); err != nil {
		t.Fatal(err)
	}
	band, ok := ticketpage.CoverBand(src.Bytes())
	if !ok {
		t.Fatal("no band")
	}
	// A map image as large as the gray 1280 × 480 maps measured (about
	// 110 KB), with room.
	d.BandJPEG = band
	d.Map, d.MapType, d.MapAttribution = make([]byte, 130<<10), "image/jpeg", "© OpenStreetMap katkıda bulunanlar"
	html, header := render(t, d)
	if total := len(html) + len(header); total > 512<<10 {
		t.Fatalf("the renderer gets %d bytes (band %d, map %d)", total, len(d.BandJPEG), len(d.Map))
	}
}

// The directions QR and its links come only with a point or an address: a
// place known by its name alone ("Online") gets none.
func TestDirectionsOnlyForAPlaceOnAMap(t *testing.T) {
	t.Parallel()
	lat, lng := 41.025376, 28.889648
	for _, tc := range []struct {
		address  string
		lat, lng *float64
		want     bool
	}{
		{"", nil, nil, false},
		{"  ", nil, nil, false},
		{"Davutpaşa Cd. No:127", nil, nil, true},
		{"", &lat, &lng, true},
	} {
		got := ticketpage.DirectionsURL("Online", tc.address, tc.lat, tc.lng)
		if (got != "") != tc.want || (ticketpage.MapsURL("Online", tc.address, tc.lat, tc.lng) != "") != tc.want {
			t.Errorf("%q %v: %q", tc.address, tc.lat != nil, got)
		}
	}
	d := baseDocument()
	d.Venue, d.VenueAddress, d.DirectionsURL = "Online", "", ticketpage.DirectionsURL("Online", "", nil, nil)
	html, _ := render(t, d)
	if strings.Contains(html, "Yol tarifi") || !strings.Contains(html, `<div class="ad">Online</div>`) {
		t.Error("a directions QR for a place known by its name alone")
	}
}
