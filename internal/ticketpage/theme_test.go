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

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/ticket/theme"
	"github.com/skylab-kulubu/core-backend/internal/ticketimage"
	"github.com/skylab-kulubu/core-backend/internal/ticketpage"
)

// The ticket page's JSON names the Event's theme and its ticket colour:
// the accent is the ticket colour, else the theme's own (ARTLAB's orange),
// else the cover's, else the club's purple (docs/event-theme.md).
func TestPageTheme(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()
	g := f.guest("elif@example.com")
	view, err := f.svc.Page(ctx, g.AccessKey)
	if err != nil {
		t.Fatal(err)
	}
	if view.Theme.Key != "kurumsal" || view.Theme.Accent != ticketpage.SiteAccent || view.TicketAccentColor != nil {
		t.Fatalf("Kurumsal: %+v %v", view.Theme, view.TicketAccentColor)
	}
	artlab, accent := "artlab", "#12AB34"
	for _, tc := range []struct {
		theme, accent *string
		key, want     string
	}{
		{&artlab, nil, "artlab", "#FF8A1F"},
		{&artlab, &accent, "artlab", "#12AB34"},
		{nil, &accent, "kurumsal", "#12AB34"},
	} {
		ev := f.ev
		ev.Theme, ev.TicketAccentColor = tc.theme, tc.accent
		ev.CoverColors = []string{"#0f766e"}
		if _, err := f.events.Update(ctx, ev); err != nil {
			t.Fatal(err)
		}
		view, err := f.svc.Page(ctx, g.AccessKey)
		if err != nil {
			t.Fatal(err)
		}
		if view.Theme.Key != tc.key || view.Theme.Accent != tc.want || (tc.accent == nil) != (view.TicketAccentColor == nil) {
			t.Errorf("%v %v: %+v %v", tc.theme, tc.accent, view.Theme, view.TicketAccentColor)
		}
	}
}

// ARTLAB's PDF: the page dark to its edges, Exo 2 embedded, its own page
// head; its own ticket image whole as the 3 : 1 poster; no ticket image,
// the theme's band, never the cover's middle; a cover region, the band.
func TestArtlabPDF(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()
	cover, poster := uuid.New(), uuid.New()
	ev := f.ev
	key := "artlab"
	ev.Theme, ev.CoverImageID = &key, &cover
	if _, err := f.events.Update(ctx, ev); err != nil {
		t.Fatal(err)
	}
	images := &bandImages{
		pages:     map[uuid.UUID][]byte{cover: halves(t, 1200, 600)},
		originals: map[uuid.UUID][]byte{poster: halves(t, 2400, 800)},
	}
	g := f.guest("elif@example.com")
	for _, tc := range []struct {
		name          string
		setting       ticketimage.Setting
		picture, afis bool
		aspect        float64
	}{
		{"no choice", ticketimage.Setting{Source: ticketimage.SourceAuto}, false, false, 0},
		{"poster", ticketimage.Setting{Source: ticketimage.SourceUpload, MediaID: &poster, Crop: &ticketimage.Crop{X: 0, Y: 0, W: 1, H: 0.13}}, true, true, 3},
		{"cover region", ticketimage.Setting{Source: ticketimage.SourceCover, CoverMediaID: &cover, Crop: &ticketimage.Crop{X: 0, Y: 0, W: 0.5, H: 0.2}}, true, false, ticketimage.BandAspect},
		{"unreadable poster", ticketimage.Setting{Source: ticketimage.SourceUpload, MediaID: new(uuid.UUID), Crop: &ticketimage.Crop{X: 0, Y: 0, W: 1, H: 0.13}}, false, false, 0},
	} {
		svc := ticketpage.New(f.store, f.events, f.users, f.signer, f.render, images, ticketpage.Config{
			Page: ticket.PageConfig{Grace: ticket.DefaultPageGrace},
		})
		svc.Now = func() time.Time { return f.now }
		svc.TicketImages = fakeTicketImages{setting: tc.setting}
		if _, _, err := svc.PDF(ctx, g.AccessKey); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		html, header := f.render.html, f.render.header
		for _, want := range []string{"@page{background:#0A1222}", "@font-face{font-family:'Exo 2'", ".qr img{width:44mm;height:44mm;display:block}"} {
			if !strings.Contains(html, want) {
				t.Errorf("%s: no %q", tc.name, want)
			}
		}
		if !strings.Contains(header, "<b>ARTLAB</b><span class=\"gri\">· SKY LAB</span>") || !strings.Contains(header, "border-bottom:1pt solid #FF8A1F") {
			t.Errorf("%s: head %s", tc.name, dataURL.ReplaceAllString(header, "…"))
		}
		if got := strings.Contains(html, `<div class="kapak afis" style="background-image:`); got != tc.afis {
			t.Errorf("%s: poster %v", tc.name, got)
		}
		if !tc.picture {
			if !strings.Contains(html, `<div class="kapak" style="background:linear-gradient(90deg,#FF8A1F,#3AA0FF)`) {
				t.Errorf("%s: not the theme's band", tc.name)
			}
			continue
		}
		cfg, err := jpeg.DecodeConfig(bytes.NewReader(bandOf(t, html)))
		if err != nil {
			t.Fatal(err)
		}
		if got := float64(cfg.Width) / float64(cfg.Height); got < tc.aspect*0.98 || got > tc.aspect*1.02 {
			t.Errorf("%s: %d × %d", tc.name, cfg.Width, cfg.Height)
		}
	}
}

// What the renderer receives for ARTLAB stays within reach of the mail
// attachment's 512 KB even for a long ticket with the largest poster (a
// 2150-pixel noise, cut to at most 144 KB), Exo 2 and a map: the inline
// files are base64 (a third larger) and the PDF embeds the fonts' used
// glyphs only, so the PDF is smaller than this.
func TestArtlabDocumentSize(t *testing.T) {
	t.Parallel()
	d := baseDocument()
	d.Theme = theme.Lookup("artlab")
	d.Days = programDays(3, 8)
	d.Description = strings.Repeat("Yapay Zeka Kampı, makine öğrenmesine yeni başlayanlar için uygulamalı bir kamptır. ", 40)
	photo := image.NewRGBA(image.Rect(0, 0, 2560, 853))
	r := rand.New(rand.NewPCG(5, 6))
	for y := range 853 {
		for x := range 2560 {
			photo.Set(x, y, color.RGBA{uint8(x / 10), uint8(r.IntN(256)), uint8(y / 4), 255})
		}
	}
	var src bytes.Buffer
	if err := jpeg.Encode(&src, photo, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	poster, ok := ticketpage.Poster(src.Bytes(), 3)
	if !ok || len(poster) > 144<<10 {
		t.Fatalf("poster of %d bytes", len(poster))
	}
	d.BandJPEG, d.BandPoster = poster, true
	d.Map, d.MapType, d.MapAttribution = make([]byte, 130<<10), "image/jpeg", "© OpenStreetMap katkıda bulunanlar"
	html, header := render(t, d)
	if total := len(html) + len(header); total > 640<<10 {
		t.Fatalf("the renderer gets %d bytes (poster %d, map %d)", total, len(poster), len(d.Map))
	}
}

// A theme's look never reaches Kurumsal, and an Event's unknown key draws
// Kurumsal.
func TestUnknownThemeIsKurumsal(t *testing.T) {
	t.Parallel()
	stored := "artlab-2019"
	if got := (event.Event{Theme: &stored}).LookTheme(); got.Key != theme.Default || got.Look != nil {
		t.Fatalf("%+v", got)
	}
}

// A Session's description ("Unvan · Kurum") is printed under its title on
// one small line, its first line, cut to fit the column; a programme
// without one draws as before (TestKurumsalTicketUnchanged).
func TestProgramNotes(t *testing.T) {
	t.Parallel()
	d := baseDocument()
	long := strings.Repeat("Çok uzun bir unvan ve kurum adı ", 8)
	d.Days = []ticketpage.ProgramDay{{Date: at("2026-11-27T00:00:00+03:00"), Sessions: []ticketpage.ProgramSession{
		session("2026-11-27T11:00:00+03:00", "2026-11-27T11:45:00+03:00", "1. Oturum", "Ayşe Kaya"),
		session("2026-11-27T11:45:00+03:00", "2026-11-27T12:30:00+03:00", "2. Oturum", "Mehmet Demir"),
		session("2026-11-27T12:30:00+03:00", "2026-11-27T13:00:00+03:00", "Ara", ""),
	}}}
	d.Days[0].Sessions[0].Note = "\n  Lead Expert   Data Scientist · Garanti BBVA Teknoloji \nikinci satır basılmaz"
	d.Days[0].Sessions[1].Note = long
	html, _ := render(t, d)
	for _, want := range []string{
		`<td class="otr">1. Oturum<span class="not">Lead Expert Data Scientist · Garanti BBVA Teknoloji</span></td><td class="kisi">Ayşe Kaya</td>`,
		`<td class="otr">Ara</td>`, "td.otr .not{display:block;font-size:7.6pt;",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("no %q", want)
		}
	}
	if strings.Contains(html, "ikinci satır") {
		t.Error("a second line printed")
	}
	cut := regexp.MustCompile(`2\. Oturum<span class="not">([^<]*)</span>`).FindStringSubmatch(html)
	if cut == nil || !strings.HasSuffix(cut[1], "…") || len([]rune(cut[1])) > ticketpage.NoteRunes[1] {
		t.Errorf("long note %q", cut)
	}
	d.Days = append(d.Days, d.Days[0])
	html, _ = render(t, d)
	if cut := regexp.MustCompile(`2\. Oturum<span class="not">([^<]*)</span>`).FindStringSubmatch(html); cut == nil || len([]rune(cut[1])) > ticketpage.NoteRunes[2] {
		t.Errorf("two days: note %q", cut)
	}
}

// The PDF's text carries no emoji (its typefaces have none; the renderer
// would draw a box): they go with the spaces they leave, the rest stays.
func TestPDFTextWithoutEmoji(t *testing.T) {
	t.Parallel()
	d := baseDocument()
	d.EventName = "🚀 8. ARTLAB: Yapay Zeka Zirvesi ✨"
	d.Description = "🚀 Yapay zekânın geleceği burada! 💫\n🤝 Tanış, konuş.\n📅 27 Kasım · ⏰ 11.00\n📍 Tarihi Hamam\n🎟️ Kayıt ücretsiz 👩‍💻"
	d.Days = []ticketpage.ProgramDay{{Sessions: []ticketpage.ProgramSession{session("2026-11-27T11:00:00+03:00", "2026-11-27T11:45:00+03:00", "1. Oturum 🎤", "Ayşe Kaya")}}}
	d.Days[0].Sessions[0].Note = "Lead 🧠 Expert · Garanti"
	html, _ := render(t, d)
	text := dataURL.ReplaceAllString(html, "")
	for _, r := range text {
		if r >= 0x1F000 || (r >= 0x2600 && r <= 0x27BF) || r == 0xFE0F || r == 0x200D || r == 0x23F0 {
			t.Fatalf("an emoji is left: %U", r)
		}
	}
	for _, want := range []string{
		"<b class=\"buyuk\">8. ARTLAB: Yapay Zeka Zirvesi</b>",
		"Yapay zekânın geleceği burada!\nTanış, konuş.\n27 Kasım · 11.00\nTarihi Hamam\nKayıt ücretsiz",
		`1. Oturum<span class="not">Lead Expert · Garanti</span>`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("no %q", want)
		}
	}
	if d.Description[:4] != "🚀" {
		t.Error("the caller's text changed")
	}
}
