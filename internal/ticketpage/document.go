package ticketpage

import (
	"bytes"
	"encoding/base64"
	"html/template"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/skylab-kulubu/core-backend/internal/brandfont"
	"github.com/skylab-kulubu/core-backend/internal/organizer"
	"github.com/skylab-kulubu/core-backend/internal/qr"
	"github.com/skylab-kulubu/core-backend/internal/ticket/theme"
)

// ClubName is how the ticket names the club, everywhere (Yusuf,
// 2026-11-22): organizer.ClubName, the one place core keeps it.
const ClubName = organizer.ClubName

// KVKKLink is the club's personal data notice ("KVKK Metni"), as the club
// site's footer links it: the footnote's one address. The ticket page's
// own link is never printed: its key also matches the holder at the door's
// attendance page (ADR 0082, decision 12), and a printed or forwarded PDF
// must not carry it.
const KVKKLink = "skyl.app/kvkk-metni"

// Document is what the PDF ticket shows (PDF v5, docs/ticket-page.md).
// Nothing else of the person: no e-mail, phone, Sky number or ticket type
// (ADR 0082). Its text is data and the footnote: no sentence tells the
// holder what to do.
type Document struct {
	EventName  string
	Start, End *time.Time
	// Venue is the place's name (the Event's location), VenueAddress its
	// street address; DirectionsURL the directions link the place's QR
	// holds.
	Venue         string
	VenueAddress  string
	DirectionsURL string
	OwnerTeam     string
	// CoOrganizers names the Event's Co-organizer teams shown outside the
	// panel (organizer.CoNames); empty draws no "Ortak düzenleyen" line.
	CoOrganizers string
	HolderName   string
	// Code is the Ticket's code as shown, XXXX-XXXX.
	Code        string
	QRToken     string
	Description string
	// Days is the programme, in order; a day without Sessions is left out.
	Days []ProgramDay
	// BandJPEG is the cover band: the cover cut to the band's shape. Without
	// it the band is the theme's band (theme.Look.Band), else BandColor
	// (the Event's ticket colour or the cover's first colour), else the
	// theme's accent. BandPoster says BandJPEG is the theme's poster
	// (theme.Look.PosterAspect), not the band.
	BandJPEG   []byte
	BandColor  string
	BandPoster bool
	// Map is the place's map image (MapType image/jpeg or image/png) and
	// the line that credits its data; none draws the place without a map.
	Map            []byte
	MapType        string
	MapAttribution string
	Theme          theme.Theme
	IssuedAt       time.Time
}

// ProgramDay is one day of the programme: its date (nil when it has none)
// and its Sessions.
type ProgramDay struct {
	Date     *time.Time
	Sessions []ProgramSession
}

// ProgramSession is one row of the programme. Note is the Session's
// description ("Unvan · Kurum"), printed under the title on one small line
// (its first line, cut to fit: NoteRunes); empty draws none.
type ProgramSession struct {
	Start, End *time.Time
	Title      string
	Speaker    string
	Note       string
}

// NoteRunes is how long a Session's note runs under its title: a day alone
// takes the page's width, two days side by side half of it each.
var NoteRunes = map[int]int{1: 96, 2: 44}

// ProgramColumns is how many days stand side by side, at most: two (Yusuf,
// 2026-11-22: three looked crowded). A third day starts a new row, under
// the first.
const ProgramColumns = 2

// istanbul is the club's time zone: every date on a ticket is in it.
var istanbul = func() *time.Location {
	loc, err := time.LoadLocation("Europe/Istanbul")
	if err != nil {
		return time.FixedZone("TRT", 3*60*60)
	}
	return loc
}()

var (
	months      = [...]string{"Ocak", "Şubat", "Mart", "Nisan", "Mayıs", "Haziran", "Temmuz", "Ağustos", "Eylül", "Ekim", "Kasım", "Aralık"}
	shortMonths = [...]string{"Oca", "Şub", "Mar", "Nis", "May", "Haz", "Tem", "Ağu", "Eyl", "Eki", "Kas", "Ara"}
	days        = [...]string{"Pazar", "Pazartesi", "Salı", "Çarşamba", "Perşembe", "Cuma", "Cumartesi"}
	shortDays   = [...]string{"Paz", "Pzt", "Sal", "Çar", "Per", "Cum", "Cmt"}
)

// ShortDate is a date as the ticket's fields show it: "12 Ara 2026 Cmt".
func ShortDate(t time.Time) string {
	t = t.In(istanbul)
	return strconv.Itoa(t.Day()) + " " + shortMonths[t.Month()-1] + " " + strconv.Itoa(t.Year()) + " " + shortDays[t.Weekday()]
}

// DayLine is a programme day's date: "12 Aralık Cumartesi".
func DayLine(t time.Time) string {
	t = t.In(istanbul)
	return strconv.Itoa(t.Day()) + " " + months[t.Month()-1] + " " + days[t.Weekday()]
}

// clock is a time of day in Istanbul, "09:30".
func clock(t time.Time) string {
	return t.In(istanbul).Format("15:04")
}

// When is the ticket's two date fields, labelled: an Event within one day
// shows its date and its hours, a longer one its start and its end, an
// undated one says so.
func When(start, end *time.Time) (firstLabel, first, secondLabel, second string) {
	switch {
	case start == nil:
		return "Tarih", "Duyurulacak", "Saat", "—"
	case end == nil:
		return "Tarih", ShortDate(*start), "Saat", clock(*start)
	case sameDay(start.In(istanbul), end.In(istanbul)):
		return "Tarih", ShortDate(*start), "Saat", clock(*start) + " – " + clock(*end)
	default:
		return "Başlangıç", ShortDate(*start) + ", " + clock(*start), "Bitiş", ShortDate(*end) + ", " + clock(*end)
	}
}

// SessionTime is a programme row's hours: "09:30–10:00", or its start alone.
func SessionTime(start, end *time.Time) string {
	switch {
	case start == nil:
		return ""
	case end == nil:
		return clock(*start)
	default:
		return clock(*start) + "–" + clock(*end)
	}
}

func sameDay(a, b time.Time) bool {
	return a.Year() == b.Year() && a.YearDay() == b.YearDay()
}

// DirectionsURL is the directions link of a place: to its point when it
// has one, else to its name and address (Google Maps' URL scheme, which
// needs no key and embeds nothing). A place with neither a point nor an
// address ("Online") has none: its name alone finds nothing to go to.
func DirectionsURL(name, address string, lat, lng *float64) string {
	if !locatable(address, lat, lng) {
		return ""
	}
	destination := strings.TrimSpace(strings.TrimSpace(name) + ", " + strings.TrimSpace(address))
	destination = strings.Trim(destination, ", ")
	if lat != nil && lng != nil {
		destination = strconv.FormatFloat(*lat, 'f', -1, 64) + "," + strconv.FormatFloat(*lng, 'f', -1, 64)
	}
	if destination == "" {
		return ""
	}
	return "https://www.google.com/maps/dir/?api=1&destination=" + url.QueryEscape(destination)
}

// MapsURL is the "Haritada aç" link of a place: a search for its point, or
// for its name and address; none for a place with neither a point nor an
// address.
func MapsURL(name, address string, lat, lng *float64) string {
	if !locatable(address, lat, lng) {
		return ""
	}
	query := strings.Trim(strings.TrimSpace(strings.TrimSpace(name)+", "+strings.TrimSpace(address)), ", ")
	if lat != nil && lng != nil {
		query = strconv.FormatFloat(*lat, 'f', -1, 64) + "," + strconv.FormatFloat(*lng, 'f', -1, 64)
	}
	if query == "" {
		return ""
	}
	return "https://www.google.com/maps/search/?api=1&query=" + url.QueryEscape(query)
}

// locatable reports whether a place can be found on a map: it has a point
// or an address.
func locatable(address string, lat, lng *float64) bool {
	return (lat != nil && lng != nil) || strings.TrimSpace(address) != ""
}

// FileName is the PDF's name: bilet-<the Event's name in ASCII>.pdf, never
// the holder's.
func FileName(eventName string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(strings.NewReplacer("İ", "i", "I", "ı").Replace(eventName)) {
		switch r {
		case 'ç':
			r = 'c'
		case 'ğ':
			r = 'g'
		case 'ı':
			r = 'i'
		case 'ö':
			r = 'o'
		case 'ş':
			r = 's'
		case 'ü':
			r = 'u'
		case 'â':
			r = 'a'
		case 'î':
			r = 'i'
		case 'û':
			r = 'u'
		}
		if r < 0x80 && (unicode.IsLetter(r) || unicode.IsDigit(r)) {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
		if b.Len() >= 60 {
			break
		}
	}
	slug := strings.Trim(b.String(), "-")
	if slug == "" {
		return "bilet.pdf"
	}
	return "bilet-" + slug + ".pdf"
}

var fontCSS = template.CSS(brandfont.FaceCSS())

type field struct {
	Label, Value string
	Wide, Large  bool
}

type programRow struct {
	Time, Title, Speaker, Note string
}

type programDay struct {
	Heading, Date string
	Rows          []programRow
}

type page struct {
	D           Document
	FontCSS     template.CSS
	CSS         template.CSS
	LookCSS     template.CSS
	Notes       bool
	Band        template.CSS
	Poster      bool
	QR          template.URL
	Fields      []field
	Days        []programDay
	Columns     int
	Single      bool
	Description string
	MapImage    template.URL
	Directions  template.URL
	Issued      string
	KVKK        string
}

// RenderHTML is the ticket's document and its page head (Chromium's header
// template, drawn in the top margin of every page), in the PDF v5 layout
// with d's theme (docs/ticket-page.md): the cover band, the ticket's fields
// beside the QR, the programme, the Event's description and its place, then
// the footnote. A long programme or description runs onto more pages; the
// fields and the QR stay on the first, and the place is never split.
func RenderHTML(d Document) (document, header string, err error) {
	t := d.Theme
	if t.Key == "" {
		t = theme.Lookup("")
		d.Theme = t
	}
	d = withoutEmoji(d)
	svg, err := qr.PlainSVG(d.QRToken)
	if err != nil {
		return "", "", err
	}
	p := page{
		D:           d,
		FontCSS:     fontCSS,
		QR:          template.URL("data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString(svg)),
		Issued:      d.IssuedAt.In(istanbul).Format("02.01.2006 15:04"),
		KVKK:        KVKKLink,
		Description: strings.TrimSpace(d.Description),
	}
	firstLabel, first, secondLabel, second := When(d.Start, d.End)
	// The club's boards' Events (and ADMIN's, or none's) are the club's
	// (Yusuf, 2026-11-23).
	organizedBy := organizer.Name(d.OwnerTeam)
	venue := strings.TrimSpace(d.Venue)
	if venue == "" {
		venue = "—"
	}
	p.Fields = []field{
		{Label: "Etkinlik", Value: d.EventName, Wide: true, Large: true},
		{Label: firstLabel, Value: first, Large: true},
		{Label: secondLabel, Value: second, Large: true},
		{Label: "Yer", Value: venue, Wide: true},
		{Label: "Katılımcı", Value: d.HolderName},
		{Label: "Düzenleyen", Value: organizedBy},
	}
	// The Co-organizer teams shown outside the panel, under the organizer
	// (docs/co-organizers.md); none draws no line.
	if co := strings.TrimSpace(d.CoOrganizers); co != "" {
		p.Fields = append(p.Fields, field{Label: "Ortak düzenleyen", Value: co, Wide: true})
	}
	for _, day := range d.Days {
		if len(day.Sessions) == 0 {
			continue
		}
		out := programDay{Heading: strconv.Itoa(len(p.Days)+1) + ". gün"}
		if day.Date != nil {
			out.Date = DayLine(*day.Date)
		}
		for _, sess := range day.Sessions {
			out.Rows = append(out.Rows, programRow{Time: SessionTime(sess.Start, sess.End), Title: sess.Title, Speaker: sess.Speaker, Note: sess.Note})
		}
		p.Days = append(p.Days, out)
	}
	p.Columns = min(len(p.Days), ProgramColumns)
	for i := range p.Days {
		for j := range p.Days[i].Rows {
			row := &p.Days[i].Rows[j]
			if row.Note = noteLine(row.Note, NoteRunes[max(p.Columns, 1)]); row.Note != "" {
				p.Notes = true
			}
		}
	}
	p.Single = len(p.Days) == 1 && len(p.Days[0].Rows) == 1
	if d.DirectionsURL != "" {
		directions, err := qr.PlainSVG(d.DirectionsURL)
		if err != nil {
			return "", "", err
		}
		p.Directions = template.URL("data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString(directions))
	}
	if len(d.Map) > 0 && (d.MapType == "image/jpeg" || d.MapType == "image/png") {
		p.MapImage = template.URL("data:" + d.MapType + ";base64," + base64.StdEncoding.EncodeToString(d.Map))
	}
	switch {
	case len(d.BandJPEG) > 0:
		p.Band = template.CSS(`background-image:url("data:image/jpeg;base64,` + base64.StdEncoding.EncodeToString(d.BandJPEG) + `")`)
		p.Poster = d.BandPoster && t.Look != nil && t.Look.PosterAspect > 0
	case t.Look != nil && t.Look.Band != "":
		p.Band = template.CSS(t.Look.Band)
	case validColor(d.BandColor):
		p.Band = template.CSS("background-color:" + d.BandColor)
	default:
		p.Band = template.CSS("background-color:" + t.Accent)
	}
	p.CSS = template.CSS(`:root{--ink:` + t.Ink + `;--muted:` + t.Muted + `;--paper:` + t.Paper + `;--rule:` + t.Rule +
		`;--accent:` + t.Accent + `}body{font-family:` + t.Sans + `}`)
	// A theme's own drawing: its typefaces beside the club's, its rules
	// after the layout's (theme.Look). Kurumsal has none, its page as
	// before.
	if t.Look != nil {
		if t.Look.FontCSS != nil {
			p.FontCSS = template.CSS(string(fontCSS) + t.Look.FontCSS())
		}
		p.LookCSS = template.CSS(t.Look.DocumentCSS)
	}
	var out bytes.Buffer
	if err := documentTemplate.Execute(&out, p); err != nil {
		return "", "", err
	}
	head, err := HeaderHTML(t, d.Code)
	if err != nil {
		return "", "", err
	}
	return out.String(), head, nil
}

// validColor is a #RRGGBB colour: what a cover's colours are.
func validColor(c string) bool {
	if len(c) != 7 || c[0] != '#' {
		return false
	}
	for _, r := range c[1:] {
		if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
			return false
		}
	}
	return true
}

type head struct {
	Club string
	Sub  string
	Code string
	Font template.CSS
	Rule template.CSS
	Ink  template.CSS
	Gray template.CSS
	CSS  template.CSS
	Mark template.HTML
}

// HeaderHTML is the page head of every page: the SKY LAB mark and the
// club's name (a theme's own mark and name, theme.Look), the Ticket's code
// and the page's number. Chromium draws it as a document of its own: it
// sees none of the page's fonts or styles.
func HeaderHTML(t theme.Theme, code string) (string, error) {
	h := head{Club: ClubName, Code: code, Font: template.CSS(t.HeadSans), Rule: template.CSS("1pt solid " + t.Accent), Ink: "#000", Gray: "#555"}
	if look := t.Look; look != nil && look.HeadMark != "" {
		h.Club, h.Sub, h.Mark = look.HeadName, look.HeadSub, template.HTML(look.HeadMark)
		h.Rule, h.Ink, h.Gray, h.CSS = template.CSS(look.HeadRule), template.CSS(look.HeadInk), template.CSS(look.HeadMuted), template.CSS(look.HeadCSS)
	} else {
		var mark strings.Builder
		mark.WriteString(`<svg width="100%" height="100%" xmlns="http://www.w3.org/2000/svg" viewBox="0 0 586.43 553.91" aria-hidden="true"><g fill="` + t.Accent + `">`)
		for _, d := range theme.MarkPaths() {
			mark.WriteString(`<path d="` + d + `"/>`)
		}
		mark.WriteString(`</g></svg>`)
		h.Mark = template.HTML(mark.String())
	}
	var out bytes.Buffer
	err := headerTemplate.Execute(&out, h)
	return out.String(), err
}

var headerTemplate = template.Must(template.New("head").Parse(`<!doctype html>
<html lang="tr"><head><meta charset="utf-8"><style>
html,body{margin:0;padding:0;-webkit-print-color-adjust:exact;print-color-adjust:exact}
.bas{width:100%;margin:0 14mm;padding:8mm 0 2.4mm;border-bottom:{{.Rule}};display:flex;align-items:center;justify-content:space-between;font-family:{{.Font}};font-size:8.5pt;color:{{.Ink}}}
.kulup{display:flex;align-items:center;gap:2mm}
.isaret{display:block;width:7mm;height:6.6mm}
.kulup b{font-size:9.5pt}
.gri{color:{{.Gray}}}
{{.CSS}}</style></head><body><div class="bas">
<div class="kulup"><span class="isaret">{{.Mark}}</span><b>{{.Club}}</b>{{if .Sub}}<span class="gri">· {{.Sub}}</span>{{end}}</div>
<div><span class="gri">Bilet kodu</span> <b>{{.Code}}</b> <span class="gri">· Sayfa <span class="pageNumber"></span>/<span class="totalPages"></span></span></div>
</div></body></html>
`))

var documentTemplate = template.Must(template.New("ticket").Parse(`<!doctype html>
<html lang="tr">
<head>
<meta charset="utf-8">
<title>Etkinlik Giriş Bileti {{.D.Code}}</title>
<style>
{{.FontCSS}}
{{.CSS}}
@page{size:A4;margin:25mm 14mm 14mm}
*{box-sizing:border-box;margin:0;padding:0}
html,body{background:var(--paper)}
body{color:var(--ink);font-size:9.5pt;line-height:1.4;-webkit-print-color-adjust:exact;print-color-adjust:exact;font-variant-numeric:tabular-nums}
.kapak{height:24mm;background-size:cover;background-position:center}
h1{font-size:14pt;font-weight:800;letter-spacing:.03em;margin:5mm 0 3.5mm}
.izgara{display:grid;grid-template-columns:1fr 1fr 54mm;border-top:.5pt solid var(--ink);border-left:.5pt solid var(--ink);break-inside:avoid}
.h{border-right:.5pt solid var(--ink);border-bottom:.5pt solid var(--ink);padding:2.3mm 3mm 2.6mm}
.h>span:not(.kod){display:block;font-size:7.5pt;color:var(--muted);margin-bottom:.4mm}
.h>b{display:block;font-size:10.5pt;font-weight:700}
.h.genis{grid-column:span 2}
.h .buyuk{font-size:13pt;font-weight:800;line-height:1.2}
.qr{grid-column:3;grid-row:1/span 4;display:flex;flex-direction:column;align-items:center;justify-content:center;gap:1.8mm}
.qr img{width:44mm;height:44mm;display:block}
.kod{font-weight:800;letter-spacing:.05em;font-size:11pt}
section{margin-top:5.5mm}
section>h2{font-size:9pt;font-weight:800;letter-spacing:.03em;padding-bottom:1.6mm;border-bottom:.5pt solid var(--ink);margin-bottom:2mm;break-after:avoid}
.gunler{display:grid;grid-template-columns:repeat(var(--sutun),1fr);gap:4mm 7mm}
.gun h3{font-size:9pt;font-weight:800;margin-bottom:1mm;break-after:avoid}
.gun h3 span{font-weight:500;color:var(--muted)}
table{width:100%;border-collapse:collapse}
tr{break-inside:avoid}
td{padding:1.25mm 0;border-bottom:.4pt solid var(--rule);vertical-align:top;font-size:8.8pt}
td.saat{width:23mm;padding-right:2.5mm;white-space:nowrap;color:#333}
td.otr{font-weight:600;padding-right:2mm}
td.kisi{color:#444;text-align:right;white-space:nowrap}
.tek td{font-size:10pt;padding:2mm 0}.tek td.saat{width:28mm}
.aciklama{font-size:9pt;line-height:1.55;color:#111;white-space:pre-line}
.yer{break-inside:avoid}
.yer .ad{font-size:10pt;font-weight:700}
.yer .adres{font-size:9pt;color:#333;margin-top:.6mm}
.yer .satir{display:flex;gap:4mm;align-items:stretch}
.yer .kutu{flex:1;min-width:0}
.yer .harita{width:100%;height:auto;display:block;border:.5pt solid var(--ink);margin-top:2mm}
.yer figure{flex:none;width:24mm;display:flex;flex-direction:column;justify-content:flex-end;font-size:7.5pt;color:var(--muted);text-align:center}
.yer figure img{width:24mm;height:24mm;display:block;margin-bottom:1mm}
.atif{font-size:6.5pt;color:var(--muted);margin-top:1mm}
.dip{margin-top:5.5mm;padding-top:2mm;border-top:.5pt solid var(--ink);font-size:7.3pt;line-height:1.55;color:#333;break-inside:avoid}
{{if .Notes}}td.otr .not{display:block;font-size:7.6pt;font-weight:400;color:var(--muted);margin-top:.3mm}
{{end}}{{.LookCSS}}</style>
</head>
<body>
<div class="kapak{{if .Poster}} afis{{end}}" style="{{.Band}}" role="img" aria-label="Etkinlik kapak görseli"></div>
<h1>ETKİNLİK GİRİŞ BİLETİ</h1>
<div class="izgara">
{{range $i, $f := .Fields}}{{if eq $i 1}}<figure class="h qr"><img alt="Bilet QR kodu" src="{{$.QR}}"><span class="kod">{{$.D.Code}}</span></figure>
{{end}}<div class="h{{if $f.Wide}} genis{{end}}"><span>{{$f.Label}}</span><b{{if $f.Large}} class="buyuk"{{end}}>{{$f.Value}}</b></div>
{{end}}</div>
{{if .Days}}<section class="program"><h2>Program</h2><div class="gunler" style="--sutun:{{.Columns}}">{{range .Days}}<div class="gun">{{if gt (len $.Days) 1}}<h3>{{.Heading}}{{if .Date}} <span>· {{.Date}}</span>{{end}}</h3>{{end}}<table{{if $.Single}} class="tek"{{end}}>{{range .Rows}}<tr><td class="saat">{{.Time}}</td><td class="otr">{{.Title}}{{if .Note}}<span class="not">{{.Note}}</span>{{end}}</td><td class="kisi">{{.Speaker}}</td></tr>{{end}}</table></div>{{end}}</div></section>
{{end}}{{if .Description}}<section class="hakkinda"><h2>Etkinlik hakkında</h2><p class="aciklama">{{.Description}}</p></section>
{{end}}<section class="yer"><h2>Etkinlik yeri</h2><div class="satir"><div class="kutu"><div class="ad">{{.D.Venue}}</div>{{if .D.VenueAddress}}<div class="adres">{{.D.VenueAddress}}</div>{{end}}{{if .MapImage}}<img class="harita" src="{{.MapImage}}" alt="Etkinlik yeri haritası">{{end}}</div>{{if .Directions}}<figure><img alt="Yol tarifi QR kodu" src="{{.Directions}}">Yol tarifi</figure>{{end}}</div>
{{if .MapImage}}{{if .D.MapAttribution}}<p class="atif">{{.D.MapAttribution}}</p>{{end}}{{end}}</section>
<footer class="dip"><p>Bu bilet kişiye özeldir. Düzenlenme: {{.Issued}}</p><p>Etkinlikte fotoğraf ve video çekilir, Kulübün kanallarında paylaşılabilir. Kişisel veriler KVKK Metni&#39;ne göre işlenir: {{.KVKK}}</p></footer>
</body>
</html>
`))
