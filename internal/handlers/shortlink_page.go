package handlers

import (
	"crypto/sha256"
	"encoding/base64"
	"strings"

	"github.com/gofiber/fiber/v3"
)

// A person who opens an unknown or disabled skyl.app link in a browser gets a
// page instead of the problem JSON (docs/short-link-miss-page.md). The page is
// one self-contained file: no font, script, image or stylesheet from
// anywhere, the scene is inline SVG and the only style is one <style> element
// that the Content-Security-Policy names by its hash. Nothing from the request
// is written into it.

const shortLinkHome = "https://yildizskylab.com"

const shortLinkPageStyle = `
:root{--bg:#171717;--fg:#f5f5f5;--border:rgb(255 255 255/14%);--ink:#e5e5e5;--soft:#a3a3a3;--faint:#737373;--brand:#e0c8e5;--on-brand:#0a0a0a;--warm:#fbbf24;--cool:#93c5fd;--alert:#f87171;--surface:var(--bg);color-scheme:dark}
@media (prefers-color-scheme: light){:root{--bg:#f6f4f1;--fg:#1c1a18;--border:rgb(0 0 0/14%);--ink:#2b2825;--soft:#6b665f;--faint:#8a847d;--brand:#8f5e98;--on-brand:#fff;--warm:#a44a07;--cool:#1d4ed8;--alert:#b91c1c;color-scheme:light}}
*{box-sizing:border-box}
html,body{margin:0}
body{background:var(--bg);color:var(--fg);font-family:ui-sans-serif,system-ui,-apple-system,"Segoe UI",Roboto,sans-serif;-webkit-font-smoothing:antialiased}
main{min-height:100vh;min-height:100dvh;display:flex;flex-direction:column;align-items:center;justify-content:center;padding:24px 16px;text-align:center}
.scene{display:block;width:100%;max-width:288px;height:auto;aspect-ratio:4/3;overflow:visible;stroke:var(--ink);fill:none;stroke-width:2;stroke-linecap:round;stroke-linejoin:round}
.scene .d{stroke-width:1.25}
.scene .s{stroke:var(--soft)}
.scene .f{stroke:var(--faint)}
.scene .m{font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;stroke:none}
.scene .fill-s{fill:var(--surface)}
.scene .fi-alert{fill:var(--alert)}
.scene .fi-brand{fill:var(--brand)}
.scene .fi-cool{fill:var(--cool)}
.scene .fi-faint{fill:var(--faint)}
.scene .fi-ink{fill:var(--ink)}
.scene .fi-on-brand{fill:var(--on-brand)}
.scene .fi-soft{fill:var(--soft)}
.scene .fi-surface{fill:var(--surface)}
.scene .fi-warm{fill:var(--warm)}
.scene .st-alert{stroke:var(--alert)}
.scene .st-ink{stroke:var(--ink)}
.scene .st-surface{stroke:var(--surface)}
.code{margin:12px 0 0;font:400 12px/1 ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;letter-spacing:.14em;color:var(--soft)}
h1{margin:10px 0 0;font-size:22px;line-height:1.25;font-weight:600;letter-spacing:-.01em;text-wrap:balance}
p.line{margin:8px 0 0;max-width:34ch;font-size:15px;line-height:1.5;color:var(--soft);text-wrap:pretty}
.btn{display:inline-flex;align-items:center;min-height:44px;margin-top:18px;padding:0 16px;font-family:inherit;font-size:14px;font-weight:500;line-height:1;color:var(--fg);text-decoration:none;border:1px solid var(--border);background:rgb(127 127 127/8%);border-radius:8px}
.btn:hover{background:rgb(127 127 127/16%)}
.btn:focus-visible{outline:2px solid var(--brand);outline-offset:2px}
.flutter{animation:flutter 3.2s ease-in-out infinite;transform-box:fill-box;transform-origin:50% 100%}
@keyframes flutter{0%,100%{transform:rotate(0)}50%{transform:rotate(-7deg) translateY(-2px)}}
.stamp{animation:stamp 4s cubic-bezier(.3,0,.2,1) infinite;transform-box:fill-box;transform-origin:center}
@keyframes stamp{0%,10%{transform:scale(1.6);opacity:0}22%,100%{transform:scale(1);opacity:1}}
.strike{stroke-dasharray:60;animation:strike 4s ease-out infinite}
@keyframes strike{0%,15%{stroke-dashoffset:60}45%,100%{stroke-dashoffset:0}}
@media (prefers-reduced-motion: reduce){.scene *{animation:none!important}}
`

// The two scenes are skyl.app's own (2026-11-24, chosen by Yusuf from the
// drafts in notes/ekran/2026-11-24/skyl-app-hata in the hub), in the line
// language of the error page prototype v5: a short link is mostly opened
// from a poster's QR. 404: a faculty notice board, a SKY LAB poster half torn
// away, its QR's corner on the floor. 410: the same board, a new SKYDAYS
// poster ("tarih: ??", "yakında") pasted over an old ARTLAB one whose
// crossed-out name and date ("27 Kasım") still show.
const shortLinkScene404 = `<svg class="scene" viewBox="0 0 240 180" aria-hidden="true" focusable="false">
<path class="s" d="M10 160H230"/>
<rect x="14" y="12" width="212" height="118" rx="4" class="fill-s"/>
<rect x="20" y="18" width="200" height="106" rx="1.5" class="d s fi-warm" fill-opacity=".1"/>
<g class="fi-warm" stroke="none" opacity=".45"><circle cx="30" cy="96" r=".8"/><circle cx="38" cy="116" r=".7"/><circle cx="60" cy="24" r=".7"/><circle cx="170" cy="118" r=".8"/><circle cx="206" cy="30" r=".7"/><circle cx="196" cy="104" r=".9"/><circle cx="52" cy="120" r=".6"/><circle cx="214" cy="72" r=".7"/><circle cx="160" cy="22" r=".6"/><circle cx="26" cy="70" r=".7"/></g><g transform="rotate(-6 42 46)"><rect x="27" y="30" width="32" height="30" rx="1.5" class="fill-s"/><path class="d f" d="M32 40H54M32 46H50M32 52H54"/><text x="43" y="37" class="m fi-soft" font-size="4" text-anchor="middle" letter-spacing=".4">DUYURU</text></g><circle class="fi-cool" cx="43" cy="31" r="2.6" stroke="none"/><circle class="fi-surface" cx="42.2" cy="30.1" r=".7" stroke="none"/><g transform="rotate(4 192 50)"><rect x="174" y="34" width="38" height="30" rx="1.5" class="fill-s"/><path class="d f" d="M179 44H207M179 50H201M179 56H205"/><text x="193" y="41" class="m fi-soft" font-size="4" text-anchor="middle" letter-spacing=".4">KULÜP ODASI</text></g><circle class="fi-warm" cx="193" cy="35" r="2.6" stroke="none"/><circle class="fi-surface" cx="192.2" cy="34.1" r=".7" stroke="none"/><defs><clipPath id="yirtik-a"><path d="M72 26H150V90L145 95L141 91L135 99L129 94L123 102L117 97L111 106L105 101L99 112L93 107L87 118L81 113L72 122Z"/></clipPath></defs>
<g transform="rotate(-2 111 74)">
<path class="fill-s" d="M72 26H150V90L145 95L141 91L135 99L129 94L123 102L117 97L111 106L105 101L99 112L93 107L87 118L81 113L72 122Z"/>
<g clip-path="url(#yirtik-a)">
<rect class="fi-brand" x="72" y="26" width="78" height="14" stroke="none"/>
<text x="111" y="35.6" class="m fi-on-brand" font-size="6" text-anchor="middle" font-weight="700" letter-spacing="1">SKY LAB</text>
<text x="78" y="50" class="m fi-ink" font-size="8.4" font-weight="700">GECEKODU</text>
<text x="78" y="58" class="m fi-soft" font-size="4.6">cuma 19:00 · kulüp odası</text>
<rect x="100" y="64" width="8.47" height="8.47" rx=".6" class="d st-ink" fill="none"/><rect class="fi-ink" x="102.82" y="66.82" width="2.82" height="2.82" stroke="none"/><rect x="123.52941176470588" y="64" width="8.47" height="8.47" rx=".6" class="d st-ink" fill="none"/><rect class="fi-ink" x="126.35" y="66.82" width="2.82" height="2.82" stroke="none"/><rect x="100" y="87.52941176470588" width="8.47" height="8.47" rx=".6" class="d st-ink" fill="none"/><rect class="fi-ink" x="102.82" y="90.35" width="2.82" height="2.82" stroke="none"/><rect class="fi-ink" x="112.24" y="64.94" width="2.82" height="2.82" stroke="none"/><rect class="fi-ink" x="116.00" y="66.82" width="2.82" height="2.82" stroke="none"/><rect class="fi-ink" x="114.12" y="70.59" width="2.82" height="2.82" stroke="none"/><rect class="fi-ink" x="119.76" y="68.71" width="2.82" height="2.82" stroke="none"/><rect class="fi-ink" x="112.24" y="74.35" width="2.82" height="2.82" stroke="none"/><rect class="fi-ink" x="117.88" y="76.24" width="2.82" height="2.82" stroke="none"/><rect class="fi-ink" x="100.94" y="76.24" width="2.82" height="2.82" stroke="none"/><rect class="fi-ink" x="104.71" y="78.12" width="2.82" height="2.82" stroke="none"/><rect class="fi-ink" x="108.47" y="76.24" width="2.82" height="2.82" stroke="none"/><rect class="fi-ink" x="112.24" y="80.00" width="2.82" height="2.82" stroke="none"/><rect class="fi-ink" x="116.00" y="81.88" width="2.82" height="2.82" stroke="none"/><rect class="fi-ink" x="121.65" y="78.12" width="2.82" height="2.82" stroke="none"/><rect class="fi-ink" x="125.41" y="76.24" width="2.82" height="2.82" stroke="none"/><rect class="fi-ink" x="129.18" y="80.00" width="2.82" height="2.82" stroke="none"/><rect class="fi-ink" x="119.76" y="83.76" width="2.82" height="2.82" stroke="none"/><rect class="fi-ink" x="123.53" y="85.65" width="2.82" height="2.82" stroke="none"/><rect class="fi-ink" x="127.29" y="89.41" width="2.82" height="2.82" stroke="none"/><rect class="fi-ink" x="116.00" y="87.53" width="2.82" height="2.82" stroke="none"/><rect class="fi-ink" x="112.24" y="85.65" width="2.82" height="2.82" stroke="none"/><rect class="fi-ink" x="121.65" y="91.29" width="2.82" height="2.82" stroke="none"/><rect class="fi-ink" x="129.18" y="93.18" width="2.82" height="2.82" stroke="none"/><rect class="fi-ink" x="125.41" y="93.18" width="2.82" height="2.82" stroke="none"/><rect class="fi-ink" x="117.88" y="93.18" width="2.82" height="2.82" stroke="none"/><rect class="fi-ink" x="112.24" y="91.29" width="2.82" height="2.82" stroke="none"/>
<text x="78" y="100" class="m fi-soft" font-size="4">kayıt için</text><text x="78" y="105" class="m fi-soft" font-size="4">okut</text>
</g>
<path d="M72 26H150V90L145 95L141 91L135 99L129 94L123 102L117 97L111 106L105 101L99 112L93 107L87 118L81 113L72 122Z"/>
</g>
<circle class="fi-alert" cx="76" cy="29" r="2.6" stroke="none"/><circle class="fi-surface" cx="75.2" cy="28.1" r=".7" stroke="none"/><circle class="fi-warm" cx="146" cy="29" r="2.6" stroke="none"/><circle class="fi-surface" cx="145.2" cy="28.1" r=".7" stroke="none"/>
<g transform="translate(152 139) rotate(-8)"><g class="flutter"><path class="fill-s" d="M0 0L6 5L10 1L16 8L22 3L28 10L28 22L0 22Z"/><rect class="fi-ink" x="9" y="12" width="3" height="3" stroke="none"/><rect class="fi-ink" x="15" y="15" width="3" height="3" stroke="none"/><rect class="fi-ink" x="21" y="13" width="3" height="3" stroke="none"/><rect class="fi-ink" x="19" y="18" width="3" height="3" stroke="none"/><rect class="fi-ink" x="4" y="16" width="3" height="3" stroke="none"/></g></g>
<path class="d f" stroke-dasharray="1.5 2.5" d="M131 104Q142 118 156 136"/>
</svg>`

const shortLinkScene410 = `<svg class="scene" viewBox="0 0 240 180" aria-hidden="true" focusable="false">
<rect x="14" y="12" width="212" height="118" rx="4" class="fill-s"/>
<rect x="20" y="18" width="200" height="106" rx="1.5" class="d s fi-warm" fill-opacity=".1"/>
<g class="fi-warm" stroke="none" opacity=".45"><circle cx="30" cy="96" r=".8"/><circle cx="38" cy="116" r=".7"/><circle cx="60" cy="24" r=".7"/><circle cx="170" cy="118" r=".8"/><circle cx="206" cy="30" r=".7"/><circle cx="196" cy="104" r=".9"/><circle cx="52" cy="120" r=".6"/><circle cx="214" cy="72" r=".7"/><circle cx="160" cy="22" r=".6"/><circle cx="26" cy="70" r=".7"/></g><g transform="rotate(4 192 50)"><rect x="174" y="34" width="38" height="30" rx="1.5" class="fill-s"/><path class="d f" d="M179 44H207M179 50H201M179 56H205"/><text x="193" y="41" class="m fi-soft" font-size="4" text-anchor="middle" letter-spacing=".4">KULÜP ODASI</text></g><circle class="fi-warm" cx="193" cy="35" r="2.6" stroke="none"/><circle class="fi-surface" cx="192.2" cy="34.1" r=".7" stroke="none"/>
<g transform="rotate(-5 68 83)"><rect x="28" y="44" width="80" height="78" rx="1" class="fill-s"/>
<rect class="fi-faint" x="28" y="44" width="80" height="11" fill-opacity=".3" stroke="none"/>
<text x="33" y="52" class="m fi-faint" font-size="4.4" font-weight="700" letter-spacing=".6">SKY LAB</text>
<path class="d f" d="M33 64H69M33 70H59"/>
<text x="33" y="88" class="m fi-soft" font-size="6.4" font-weight="700">ARTLAB</text>
<text x="33" y="98" class="m fi-soft" font-size="6.4" font-weight="700">27 Kasım</text>
<path stroke-width="1.4" class="strike st-alert" d="M31 86H58M31 96H66"/>
</g>
<g transform="rotate(2 122 74)"><path class="fill-s" d="M80 22H164V126H80Z"/>
<rect class="fi-brand" x="81" y="23" width="82" height="13" stroke="none"/>
<text x="122" y="31.6" class="m fi-on-brand" font-size="6" text-anchor="middle" font-weight="700" letter-spacing="1">SKY LAB</text>
<text x="86" y="49" class="m fi-ink" font-size="9" font-weight="700" letter-spacing=".4">SKYDAYS</text>
<text x="86" y="59" class="m fi-soft" font-size="5">tarih: ??</text>
<path class="d f" d="M86 65H128"/>
<rect x="86" y="74" width="7.94" height="7.94" rx=".6" class="d st-ink" fill="none"/><rect class="fi-ink" x="88.65" y="76.65" width="2.65" height="2.65" stroke="none"/><rect x="108.05882352941177" y="74" width="7.94" height="7.94" rx=".6" class="d st-ink" fill="none"/><rect class="fi-ink" x="110.71" y="76.65" width="2.65" height="2.65" stroke="none"/><rect x="86" y="96.05882352941177" width="7.94" height="7.94" rx=".6" class="d st-ink" fill="none"/><rect class="fi-ink" x="88.65" y="98.71" width="2.65" height="2.65" stroke="none"/><rect class="fi-ink" x="97.47" y="74.88" width="2.65" height="2.65" stroke="none"/><rect class="fi-ink" x="101.00" y="76.65" width="2.65" height="2.65" stroke="none"/><rect class="fi-ink" x="99.24" y="80.18" width="2.65" height="2.65" stroke="none"/><rect class="fi-ink" x="104.53" y="78.41" width="2.65" height="2.65" stroke="none"/><rect class="fi-ink" x="97.47" y="83.71" width="2.65" height="2.65" stroke="none"/><rect class="fi-ink" x="102.76" y="85.47" width="2.65" height="2.65" stroke="none"/><rect class="fi-ink" x="86.88" y="85.47" width="2.65" height="2.65" stroke="none"/><rect class="fi-ink" x="90.41" y="87.24" width="2.65" height="2.65" stroke="none"/><rect class="fi-ink" x="93.94" y="85.47" width="2.65" height="2.65" stroke="none"/><rect class="fi-ink" x="97.47" y="89.00" width="2.65" height="2.65" stroke="none"/><rect class="fi-ink" x="101.00" y="90.76" width="2.65" height="2.65" stroke="none"/><rect class="fi-ink" x="106.29" y="87.24" width="2.65" height="2.65" stroke="none"/><rect class="fi-ink" x="109.82" y="85.47" width="2.65" height="2.65" stroke="none"/><rect class="fi-ink" x="113.35" y="89.00" width="2.65" height="2.65" stroke="none"/><rect class="fi-ink" x="104.53" y="92.53" width="2.65" height="2.65" stroke="none"/><rect class="fi-ink" x="108.06" y="94.29" width="2.65" height="2.65" stroke="none"/><rect class="fi-ink" x="111.59" y="97.82" width="2.65" height="2.65" stroke="none"/><rect class="fi-ink" x="101.00" y="96.06" width="2.65" height="2.65" stroke="none"/><rect class="fi-ink" x="97.47" y="94.29" width="2.65" height="2.65" stroke="none"/><rect class="fi-ink" x="106.29" y="99.59" width="2.65" height="2.65" stroke="none"/><rect class="fi-ink" x="113.35" y="101.35" width="2.65" height="2.65" stroke="none"/><rect class="fi-ink" x="109.82" y="101.35" width="2.65" height="2.65" stroke="none"/><rect class="fi-ink" x="102.76" y="101.35" width="2.65" height="2.65" stroke="none"/><rect class="fi-ink" x="97.47" y="99.59" width="2.65" height="2.65" stroke="none"/>
</g>
<circle class="fi-cool" cx="32" cy="47" r="2.6" stroke="none"/><circle class="fi-surface" cx="31.2" cy="46.1" r=".7" stroke="none"/><circle class="fi-alert" cx="84" cy="25" r="2.6" stroke="none"/><circle class="fi-surface" cx="83.2" cy="24.1" r=".7" stroke="none"/><circle class="fi-cool" cx="160" cy="27" r="2.6" stroke="none"/><circle class="fi-surface" cx="159.2" cy="26.1" r=".7" stroke="none"/><circle class="fi-warm" cx="82" cy="124" r="2.6" stroke="none"/><circle class="fi-surface" cx="81.2" cy="123.1" r=".7" stroke="none"/>
<g class="stamp"><g transform="rotate(-8 141 88)"><rect class="fi-brand st-surface" x="122" y="82" width="38" height="12" rx="6" stroke-width="1.2"/><text x="141" y="90.2" class="m fi-on-brand" font-size="5.6" text-anchor="middle" font-weight="700">yakında</text></g></g>
</svg>`

var (
	shortLinkPageCSP = "default-src 'none'; style-src 'sha256-" + shortLinkStyleHash() + "'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
	shortLinkPage404 = shortLinkPage("404", "Bu sayfa yok",
		"Afişteki bağlantı eksik okunmuş olabilir; QR'ı yeniden okut ya da adresi kontrol et.", shortLinkScene404)
	shortLinkPage410 = shortLinkPage("410", "Bu bağlantının süresi doldu",
		"Etkinlik bitti, bağlantı da onunla kapandı; güncel etkinlikler ana sayfada.", shortLinkScene410)
)

func shortLinkStyleHash() string {
	sum := sha256.Sum256([]byte(shortLinkPageStyle))
	return base64.StdEncoding.EncodeToString(sum[:])
}

func shortLinkPage(code, title, line, scene string) []byte {
	var b strings.Builder
	b.WriteString(`<!doctype html>
<html lang="tr">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="light dark">
<meta name="robots" content="noindex">
<title>`)
	b.WriteString(title)
	b.WriteString(` · SKY LAB</title>
<style>`)
	b.WriteString(shortLinkPageStyle)
	b.WriteString(`</style>
</head>
<body>
<main>
`)
	b.WriteString(scene)
	b.WriteString(`
<p class="code">`)
	b.WriteString(code)
	b.WriteString(`</p>
<h1>`)
	b.WriteString(title)
	b.WriteString(`</h1>
<p class="line">`)
	b.WriteString(line)
	b.WriteString(`</p>
<a class="btn" href="` + shortLinkHome + `">Ana sayfaya dön</a>
</main>
</body>
</html>
`)
	return []byte(b.String())
}

// wantsShortLinkPage is true when the caller prefers HTML to JSON: a browser
// does, a client that asks for JSON or sends no Accept (sky-app, curl) does
// not, and a tie goes to JSON.
func wantsShortLinkPage(c fiber.Ctx) bool {
	return c.Accepts(fiber.MIMEApplicationJSON, "application/problem+json", fiber.MIMETextHTML) == fiber.MIMETextHTML
}

// shortLinkMissPage answers a browser's hop to an unknown alias (404) or to
// one whose link was disabled (410).
func shortLinkMissPage(c fiber.Ctx, gone bool) error {
	status, page := fiber.StatusNotFound, shortLinkPage404
	if gone {
		status, page = fiber.StatusGone, shortLinkPage410
	}
	c.Set(fiber.HeaderCacheControl, "no-store")
	c.Set(fiber.HeaderContentSecurityPolicy, shortLinkPageCSP)
	c.Set(fiber.HeaderXContentTypeOptions, "nosniff")
	c.Set(fiber.HeaderReferrerPolicy, "no-referrer")
	c.Set(fiber.HeaderContentType, fiber.MIMETextHTMLCharsetUTF8)
	return c.Status(status).Send(page)
}

// ShortLinkRoot is /v1/go with no alias (and /v1/go/): the club site, for a
// person and a program alike. skyl.app/ itself arrives as / (ShortLinkBareRoot).
func ShortLinkRoot(c fiber.Ctx) error {
	c.Set(fiber.HeaderCacheControl, "no-store")
	c.Set(fiber.HeaderLocation, shortLinkHome)
	return c.SendStatus(fiber.StatusFound)
}

// ShortLinkBareRoot is skyl.app/ as it reaches Core: the proxy turns
// skyl.app/{path} into /v1/go/{path} only when there is a path, so the bare
// root arrives as /. A browser goes to the club site as from /v1/go; any
// other caller falls through to the 404 problem JSON it got before.
func ShortLinkBareRoot(c fiber.Ctx) error {
	c.Vary(fiber.HeaderAccept)
	if !wantsShortLinkPage(c) {
		return c.Next()
	}
	return ShortLinkRoot(c)
}
