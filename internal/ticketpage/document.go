package ticketpage

import (
	"bytes"
	"encoding/base64"
	"html"
	"html/template"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/skylab-kulubu/core-backend/internal/qr"
	"github.com/skylab-kulubu/core-backend/internal/ticket/theme"
)

// The card's size in millimetres; the guilloche is drawn to it.
const (
	cardWidth  = 194
	cardHeight = 114
)

// Document is what the PDF ticket shows. Nothing else: no e-mail, phone,
// Sky number or ticket type (ADR 0082).
type Document struct {
	EventName  string
	Start, End *time.Time
	Venue      string
	OwnerTeam  string
	HolderName string
	// Code is the Ticket's code as shown, XXXX-XXXX: the serial number.
	Code    string
	QRToken string
	Theme   theme.Theme
	// TeamLogo is the organizing team's logo for the second seal; empty
	// draws the club's seal alone.
	TeamLogo     []byte
	TeamLogoType string
	IssuedAt     time.Time
}

// istanbul is the club's time zone: every date on a ticket is in it.
var istanbul = func() *time.Location {
	loc, err := time.LoadLocation("Europe/Istanbul")
	if err != nil {
		return time.FixedZone("TRT", 3*60*60)
	}
	return loc
}()

var (
	months = [...]string{"Ocak", "Şubat", "Mart", "Nisan", "Mayıs", "Haziran", "Temmuz", "Ağustos", "Eylül", "Ekim", "Kasım", "Aralık"}
	days   = [...]string{"Pazar", "Pazartesi", "Salı", "Çarşamba", "Perşembe", "Cuma", "Cumartesi"}
)

// DateLine and TimeLine are the ticket's date and time, in Turkish, in
// Istanbul's time.
func DateLine(start, end *time.Time) string {
	if start == nil {
		return "Tarih duyurulacak"
	}
	s := start.In(istanbul)
	if end == nil || sameDay(s, end.In(istanbul)) {
		return strconv.Itoa(s.Day()) + " " + months[s.Month()-1] + " " + strconv.Itoa(s.Year()) + ", " + days[s.Weekday()]
	}
	e := end.In(istanbul)
	switch {
	case s.Year() == e.Year() && s.Month() == e.Month():
		return strconv.Itoa(s.Day()) + "–" + strconv.Itoa(e.Day()) + " " + months[e.Month()-1] + " " + strconv.Itoa(e.Year())
	case s.Year() == e.Year():
		return strconv.Itoa(s.Day()) + " " + months[s.Month()-1] + " – " + strconv.Itoa(e.Day()) + " " + months[e.Month()-1] + " " + strconv.Itoa(e.Year())
	default:
		return strconv.Itoa(s.Day()) + " " + months[s.Month()-1] + " " + strconv.Itoa(s.Year()) + " – " +
			strconv.Itoa(e.Day()) + " " + months[e.Month()-1] + " " + strconv.Itoa(e.Year())
	}
}

func TimeLine(start, end *time.Time) string {
	if start == nil {
		return "—"
	}
	s := start.In(istanbul)
	if end == nil {
		return s.Format("15:04")
	}
	return s.Format("15:04") + " – " + end.In(istanbul).Format("15:04")
}

func sameDay(a, b time.Time) bool {
	return a.Year() == b.Year() && a.YearDay() == b.YearDay()
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

// sealSVG is the club's seal: a double ring, the ring text and the SKY LAB
// mark, slightly turned as a stamp is.
func sealSVG(t theme.Theme, ownerTeam, id string) string {
	ring := "YILDIZ SKY LAB • ETKİNLİK GİRİŞ BİLETİ • "
	if ownerTeam != "" {
		ring = "YILDIZ SKY LAB • " + strings.ToUpper(ownerTeam) + " • ETKİNLİK GİRİŞ BİLETİ • "
	}
	var b strings.Builder
	b.WriteString(`<svg class="seal" viewBox="0 0 100 100" aria-hidden="true" xmlns="http://www.w3.org/2000/svg">`)
	b.WriteString(`<g transform="rotate(-9 50 50)" fill="none" stroke="` + t.Seal + `">`)
	b.WriteString(`<circle cx="50" cy="50" r="47.5" stroke-width="1.6"/><circle cx="50" cy="50" r="44.6" stroke-width=".5"/>`)
	b.WriteString(`<circle cx="50" cy="50" r="30.5" stroke-width=".8"/><circle cx="50" cy="50" r="29" stroke-width=".3"/>`)
	b.WriteString(`<path id="` + id + `" d="M50 12.6a37.4 37.4 0 1 1-.01 0" stroke="none"/>`)
	b.WriteString(`<text fill="` + t.Seal + `" stroke="none" font-size="6.6" font-weight="700" letter-spacing=".6" font-family="` + html.EscapeString(t.Sans) + `">`)
	b.WriteString(`<textPath href="#` + id + `" textLength="232" lengthAdjust="spacing">` + html.EscapeString(ring) + `</textPath></text>`)
	scale := 34 / theme.MarkWidth
	b.WriteString(`<g transform="translate(33 34.2) scale(` + strconv.FormatFloat(scale, 'f', 5, 64) + `)" fill="` + t.Seal + `" stroke="none" fill-rule="evenodd">`)
	for _, d := range theme.MarkPaths() {
		b.WriteString(`<path d="` + d + `"/>`)
	}
	b.WriteString(`</g></g></svg>`)
	return b.String()
}

// teamSealSVG is the organizing team's seal: its logo in a ring with its
// name.
func teamSealSVG(t theme.Theme, ownerTeam string, logo template.URL) string {
	var b strings.Builder
	b.WriteString(`<svg class="seal seal-team" viewBox="0 0 100 100" aria-hidden="true" xmlns="http://www.w3.org/2000/svg">`)
	b.WriteString(`<defs><clipPath id="teamclip"><circle cx="50" cy="50" r="28"/></clipPath></defs>`)
	b.WriteString(`<g transform="rotate(7 50 50)" fill="none" stroke="` + t.Seal + `">`)
	b.WriteString(`<circle cx="50" cy="50" r="47.5" stroke-width="1.6"/><circle cx="50" cy="50" r="44.6" stroke-width=".5"/><circle cx="50" cy="50" r="30.5" stroke-width=".8"/>`)
	b.WriteString(`<path id="teamring" d="M50 12.6a37.4 37.4 0 1 1-.01 0" stroke="none"/>`)
	b.WriteString(`<text fill="` + t.Seal + `" stroke="none" font-size="7" font-weight="700" letter-spacing="1" font-family="` + html.EscapeString(t.Sans) + `">`)
	b.WriteString(`<textPath href="#teamring" textLength="232" lengthAdjust="spacing">` + html.EscapeString(strings.Repeat("DÜZENLEYEN • "+strings.ToUpper(ownerTeam)+" • ", 2)) + `</textPath></text>`)
	b.WriteString(`<image href="` + html.EscapeString(string(logo)) + `" x="24" y="24" width="52" height="52" preserveAspectRatio="xMidYMid meet" clip-path="url(#teamclip)"/>`)
	b.WriteString(`</g></svg>`)
	return b.String()
}

var logoTypes = map[string]bool{"image/png": true, "image/jpeg": true, "image/webp": true, "image/svg+xml": true}

type page struct {
	D          Document
	Date, Time string
	Issued     string
	NameClass  string
	CSS        template.CSS
	QR         template.URL
	HeadSeal   template.HTML
	Seal       template.HTML
	TeamSeal   template.HTML
	Micro      string
}

// RenderHTML is the ticket's one A4 page, in the Kurumsal layout with d's
// theme: an official document's ticket with its stub (docs/ticket-page.md).
func RenderHTML(d Document) (string, error) {
	svg, err := qr.PlainSVG(d.QRToken)
	if err != nil {
		return "", err
	}
	t := d.Theme
	if t.Key == "" {
		t = theme.Lookup("")
		d.Theme = t
	}
	ground := theme.Guilloche(strings.ReplaceAll(d.Code, "-", ""), cardWidth, cardHeight, t.Accent)
	p := page{
		D:        d,
		Date:     DateLine(d.Start, d.End),
		Time:     TimeLine(d.Start, d.End),
		Issued:   d.IssuedAt.In(istanbul).Format("02.01.2006 15:04"),
		QR:       template.URL("data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString(svg)),
		HeadSeal: template.HTML(sealSVG(t, d.OwnerTeam, "headring")),
		Seal:     template.HTML(sealSVG(t, d.OwnerTeam, "sealring")),
		Micro:    strings.Repeat(strings.ToUpper(d.EventName)+" · SKY LAB · "+d.Code+" · ", 14),
	}
	switch n := len([]rune(d.EventName)); {
	case n > 70:
		p.NameClass = "name-s"
	case n > 40:
		p.NameClass = "name-m"
	}
	if len(d.TeamLogo) > 0 && logoTypes[d.TeamLogoType] && d.OwnerTeam != "" {
		logo := template.URL("data:" + d.TeamLogoType + ";base64," + base64.StdEncoding.EncodeToString(d.TeamLogo))
		p.TeamSeal = template.HTML(teamSealSVG(t, d.OwnerTeam, logo))
	}
	p.CSS = template.CSS(`:root{--ink:` + t.Ink + `;--muted:` + t.Muted + `;--accent:` + t.Accent + `;--paper:` + t.Paper +
		`;--tint:` + t.Tint + `;--seal:` + t.Seal + `}` +
		`.ground{background-image:url("data:image/svg+xml;base64,` + base64.StdEncoding.EncodeToString([]byte(ground)) + `")}` +
		`body{font-family:` + t.Sans + `}.serif{font-family:` + t.Serif + `}.mono{font-family:` + t.Mono + `}`)
	var out bytes.Buffer
	if err := documentTemplate.Execute(&out, p); err != nil {
		return "", err
	}
	return out.String(), nil
}

var documentTemplate = template.Must(template.New("ticket").Parse(`<!doctype html>
<html lang="tr">
<head>
<meta charset="utf-8">
<title>{{.D.EventName}} bileti</title>
<style>
{{.CSS}}
@page{size:210mm 148mm;margin:0}
*{box-sizing:border-box}
html,body{margin:0;padding:0}
body{width:210mm;height:148mm;overflow:hidden;background:var(--paper);color:var(--ink);-webkit-print-color-adjust:exact;print-color-adjust:exact}
.sheet{padding:7mm 8mm 0}
.head{display:flex;justify-content:space-between;align-items:flex-end;padding-bottom:1.8mm;border-bottom:.45mm solid var(--ink);margin-bottom:.7mm}
.head-rule{border-bottom:.15mm solid var(--ink);margin-bottom:3mm}
.org{display:flex;align-items:center;gap:3mm}
.org svg{width:9.5mm;height:9.5mm}
.org-name{font-size:13.5pt;font-weight:700;letter-spacing:.14em;line-height:1}
.org-sub{font-size:6.6pt;letter-spacing:.22em;color:var(--muted);font-weight:700;margin-top:1.4mm}
.doc-title{text-align:right}
.doc-title b{display:block;font-size:10pt;letter-spacing:.2em}
.doc-title span{display:block;font-size:6.6pt;letter-spacing:.24em;color:var(--muted);margin-top:1mm}
.ticket{position:relative;width:194mm;height:114mm;border:.5mm solid var(--ink);border-radius:3mm;background:var(--tint);display:flex;overflow:hidden}
.ground{position:absolute;inset:0;background-size:100% 100%;background-repeat:no-repeat}
.frame{position:absolute;inset:2.2mm;border:.2mm solid var(--accent);border-radius:1.8mm}
.micro{position:absolute;left:4mm;right:4mm;height:2mm;overflow:hidden;white-space:nowrap;font-size:3.3pt;line-height:2mm;letter-spacing:.06em;color:var(--accent);opacity:.85}
.micro.top{top:2.6mm}.micro.bottom{bottom:2.6mm}
.body{position:relative;width:142mm;height:100%;padding:7mm 6mm 6.5mm 8mm;display:grid;grid-template-columns:79mm 49mm;grid-template-rows:auto 1fr;column-gap:0}
.kicker{grid-column:1 / 3;display:flex;justify-content:space-between;align-items:baseline;margin-bottom:2.4mm}
.kicker b{font-size:7.4pt;letter-spacing:.22em;color:var(--accent)}
.kicker span{font-size:6pt;letter-spacing:.2em;color:var(--muted)}
.left{display:flex;flex-direction:column}
.event{font-size:19pt;line-height:1.12;font-weight:700;margin:0 0 3mm;max-height:21mm;overflow:hidden}
.event.name-m{font-size:16pt}.event.name-s{font-size:12.5pt}
.fields{display:grid;grid-template-columns:1.25fr 1fr;column-gap:4mm;row-gap:2.2mm}
.field{border-top:.2mm solid rgba(20,33,61,.35);padding-top:1.2mm}
.field.wide{grid-column:1 / 3}
.label{font-size:5.8pt;letter-spacing:.18em;color:var(--muted);font-weight:700;text-transform:uppercase}
.value{font-size:10pt;font-weight:600;margin-top:.8mm;line-height:1.2}
.holder .value{font-size:14pt;font-weight:700;letter-spacing:.01em}
.serial .value{color:var(--accent);letter-spacing:.06em}
.stamp-row{margin-top:auto;display:flex;align-items:center;gap:2mm}
.seal{width:24mm;height:24mm;flex:none;opacity:.92}
.seal-team{width:20mm;height:20mm;margin-left:-5mm}
.note{font-size:7.6pt;line-height:1.35;color:var(--ink);font-weight:600;margin-left:1mm}
.right{display:flex;flex-direction:column;align-items:center;justify-content:flex-start}
.qr{background:#fff;border:.3mm solid var(--ink);border-radius:1.5mm;padding:1mm;width:47mm;height:47mm}
.qr img{display:block;width:100%;height:100%}
.qr-caption{font-size:5.8pt;letter-spacing:.18em;color:var(--muted);font-weight:700;margin-top:2mm;text-transform:uppercase}
.code{font-size:16pt;font-weight:700;letter-spacing:.1em;margin-top:.8mm}
.stub{position:relative;flex:1;border-left:.45mm dashed var(--ink);padding:7mm 5mm 6.5mm 6mm;display:flex;flex-direction:column;align-items:center;text-align:center}
.stub::before,.stub::after{content:"";position:absolute;left:-3.2mm;width:6mm;height:6mm;border-radius:50%;background:var(--paper);border:.5mm solid var(--ink)}
.stub::before{top:-3.5mm}.stub::after{bottom:-3.5mm}
.stub-label{font-size:6.6pt;letter-spacing:.3em;color:var(--accent);font-weight:700}
.stub-label span{display:block;font-size:5.4pt;letter-spacing:.2em;color:var(--muted);margin-top:.6mm}
.stub .qr{width:28mm;height:28mm;margin-top:3.4mm;padding:.7mm}
.stub .code{font-size:11pt;margin-top:2.4mm}
.stub-event{font-size:8pt;font-weight:700;line-height:1.25;margin-top:4mm;max-height:12mm;overflow:hidden}
.stub-date{font-size:7pt;color:var(--muted);margin-top:1.6mm;line-height:1.3}
.stub-foot{margin-top:auto;font-size:5.4pt;letter-spacing:.16em;color:var(--muted)}
.foot{margin-top:2.2mm;display:flex;justify-content:space-between;font-size:6.6pt;color:var(--muted);letter-spacing:.04em}
</style>
</head>
<body>
<main class="sheet">
<header class="head">
<div class="org">{{.HeadSeal}}<div><div class="org-name serif">YILDIZ SKY LAB</div><div class="org-sub">ETKİNLİK BİLETİ BELGESİ</div></div></div>
<div class="doc-title"><b>ETKİNLİK GİRİŞ BİLETİ</b><span>EVENT ADMISSION TICKET</span></div>
</header>
<div class="head-rule"></div>
<section class="ticket" aria-label="Bilet">
<div class="ground" aria-hidden="true"></div>
<div class="frame" aria-hidden="true"></div>
<div class="micro top mono" aria-hidden="true">{{.Micro}}</div>
<div class="micro bottom mono" aria-hidden="true">{{.Micro}}</div>
<div class="body">
<div class="kicker"><b>GİRİŞ BİLETİ</b><span>SERİ NO {{.D.Code}}</span></div>
<div class="left">
<h1 class="event serif {{.NameClass}}">{{.D.EventName}}</h1>
<div class="fields">
<div class="field wide holder"><div class="label">Ad Soyad · Holder</div><div class="value">{{.D.HolderName}}</div></div>
<div class="field"><div class="label">Tarih · Date</div><div class="value">{{.Date}}</div></div>
<div class="field"><div class="label">Saat · Time</div><div class="value">{{.Time}}</div></div>
<div class="field wide"><div class="label">Yer · Venue</div><div class="value">{{if .D.Venue}}{{.D.Venue}}{{else}}—{{end}}</div></div>
<div class="field wide"><div class="label">Düzenleyen · Organizer</div><div class="value">{{if .D.OwnerTeam}}{{.D.OwnerTeam}}{{else}}SKY LAB{{end}}</div></div>
</div>
<div class="stamp-row">{{.Seal}}{{.TeamSeal}}<p class="note">Kapıda QR'ı göster.<br>Okunmazsa bilet kodunu söyle.</p></div>
</div>
<div class="right">
<div class="qr"><img alt="Bilet QR kodu" src="{{.QR}}"></div>
<div class="qr-caption">Bilet QR kodu</div>
<div class="code mono">{{.D.Code}}</div>
</div>
</div>
<aside class="stub" aria-label="Koçan">
<div class="stub-label">KOÇAN<span>STUB</span></div>
<div class="qr"><img alt="Bilet QR kodu" src="{{.QR}}"></div>
<div class="code mono">{{.D.Code}}</div>
<div class="stub-event serif">{{.D.EventName}}</div>
<div class="stub-date">{{.Date}}<br>{{.Time}}</div>
<div class="stub-foot">YILDIZ SKY LAB</div>
</aside>
</section>
<footer class="foot"><span>Seri no {{.D.Code}} · Düzenlenme {{.Issued}}</span><span>yildizskylab.com</span></footer>
</main>
</body>
</html>
`))
