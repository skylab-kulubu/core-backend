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
.scene .fi-soft{fill:var(--soft)}
.scene .fi-surface{fill:var(--surface)}
.scene .fi-warm{fill:var(--warm)}
.scene .st-alert{stroke:var(--alert)}
.scene .st-brand{stroke:var(--brand)}
.scene .st-cool{stroke:var(--cool)}
.scene .st-ink{stroke:var(--ink)}
.code{margin:12px 0 0;font:400 12px/1 ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;letter-spacing:.14em;color:var(--soft)}
h1{margin:10px 0 0;font-size:22px;line-height:1.25;font-weight:600;letter-spacing:-.01em;text-wrap:balance}
p.line{margin:8px 0 0;max-width:34ch;font-size:15px;line-height:1.5;color:var(--soft);text-wrap:pretty}
.btn{display:inline-flex;align-items:center;min-height:44px;margin-top:18px;padding:0 16px;font-family:inherit;font-size:14px;font-weight:500;line-height:1;color:var(--fg);text-decoration:none;border:1px solid var(--border);background:rgb(127 127 127/8%);border-radius:8px}
.btn:hover{background:rgb(127 127 127/16%)}
.btn:focus-visible{outline:2px solid var(--brand);outline-offset:2px}
.selfpivot{transform-box:fill-box;transform-origin:center}
.tw{animation:tw 3s ease-in-out infinite}
.blink{animation:blink 1.1s steps(1) infinite}
.d2{animation-delay:-1.8s}
@keyframes tw{50%{opacity:.25}}
@keyframes blink{50%{opacity:0}}
.scene .glow{fill:var(--cool);fill-opacity:.1;animation:glow 3.4s ease-in-out infinite}
@keyframes glow{50%{opacity:.35}}
@media (prefers-color-scheme: light){.scene .glow{fill-opacity:.06}}
.gk-type{animation:gk-type 5s steps(1) infinite}
@keyframes gk-type{0%,12%{opacity:0}13%,100%{opacity:1}}
.fly{animation:fly 4.2s ease-in infinite}
@keyframes fly{0%,30%{transform:translate(0,0);opacity:1}85%{transform:translate(14px,-26px) rotate(18deg);opacity:0}100%{opacity:0}}
.puff{animation:puff 2.4s ease-out infinite both}
@keyframes puff{0%{opacity:0;transform:translateY(3px)}40%{opacity:1}100%{opacity:0;transform:translateY(-9px)}}
.spin{animation:spin 6s linear infinite}
@keyframes spin{to{transform:rotate(360deg)}}
.stamp{animation:stamp 4s cubic-bezier(.3,0,.2,1) infinite}
@keyframes stamp{0%,10%{transform:scale(1.6);opacity:0}22%,100%{transform:scale(1);opacity:1}}
@media (prefers-reduced-motion: reduce){.scene *{animation:none!important}}
`

// The two scenes come from the error page prototype v5: 404-A (GECEKODU's
// desk at 03:47) and 410-A, its labels turned to YıldızJam's sign-up link so
// they match the sentence. The 404 copy is v4's as approved; the 410
// sentence waits for approval.
const shortLinkScene404 = `<svg class="scene" viewBox="0 0 240 180" aria-hidden="true" focusable="false"><path class="s" d="M10 146H230"/>
<rect x="14" y="14" width="46" height="36" rx="3" class="fill-s"/><path class="d f" d="M37 14V50M14 32H60"/>
<circle class="fi-warm" cx="27" cy="24" r="5" stroke="none"/><circle class="fi-surface" cx="29.5" cy="22.4" r="4.4" stroke="none"/><path class="tw fi-warm" d="M50 20.2L50.58 21.42L51.8 22L50.58 22.58L50 23.8L49.42 22.58L48.2 22L49.42 21.42Z" stroke="none"/><path class="tw d2 fi-warm" d="M46 40.6L46.45 41.55L47.4 42L46.45 42.45L46 43.4L45.55 42.45L44.6 42L45.55 41.55Z" stroke="none"/>
<text x="196" y="19" class="m fi-faint" font-size="5.5" text-anchor="middle" letter-spacing=".8">CUMA GECESİ</text>
<rect x="168" y="24" width="56" height="24" rx="5" class="fill-s"/>
<text x="196" y="41" text-anchor="middle" class="m fi-ink" font-size="13" font-weight="700">03<tspan class="blink">:</tspan>47</text>
<rect x="88" y="64" width="90" height="72" rx="5" class="fill-s"/>
<rect x="93" y="69" width="80" height="62" rx="2" class="d f"/><rect class="glow" x="93" y="69" width="80" height="62" rx="2" stroke="none"/>
<text x="98" y="80" class="m fi-soft" font-size="6" text-anchor="start">$ cd sayfa</text>
<g class="gk-type"><text x="133" y="110" class="m fi-alert" font-size="23" text-anchor="middle" font-weight="700">404</text><text x="133" y="122" class="m fi-soft" font-size="5.4" text-anchor="middle">bulunamadı</text></g>
<path class="fill-s" d="M80 136H186L193 145H73Z"/><path class="d f" d="M122 140.5H144"/>
<g class="fly"><g transform="translate(182 60) rotate(14)"><path class="d s fill-s" stroke-dasharray="2 2" d="M0 0H10L14 4V18H0Z"/><path class="d f" d="M3 7h7M3 11h7M3 15h5"/></g></g>
<g><path class="fill-s" d="M4 146L7 120H69L72 146Z"/>
<g><circle cx="20" cy="133" r="9.5" class="d s fill-s"/>
<circle class="fi-warm" cx="15.25" cy="128.44" r="1.9" stroke="none"/>
<circle class="fi-surface" cx="16.11" cy="127.68" r="1.62" stroke="none"/>
<path class="fi-warm" d="M20.48 125.4L20.78 126.05L21.43 126.35L20.78 126.65L20.48 127.3L20.17 126.65L19.53 126.35L20.17 126.05Z" stroke="none"/><path class="fi-warm" d="M24.75 127.68L25.05 128.33L25.7 128.63L25.05 128.93L24.75 129.58L24.45 128.93L23.8 128.63L24.45 128.33Z" stroke="none"/><path class="fi-warm" d="M26.27 132.24L26.51 132.76L27.03 133L26.51 133.24L26.27 133.76L26.03 133.24L25.51 133L26.03 132.76Z" stroke="none"/><path class="fi-warm" d="M13.54 131.86L13.78 132.38L14.3 132.62L13.78 132.86L13.54 133.38L13.3 132.86L12.78 132.62L13.3 132.38Z" stroke="none"/>
<path class="fi-cool st-cool" fill-opacity=".35" stroke-width="0.8" d="M15.63 140.6C15.25 133.95 16.96 129.96 20 129.96C23.04 129.96 24.75 133.95 24.37 140.6"/>
<ellipse class="fi-surface" cx="20" cy="133.29" rx="2" ry="1.9" stroke="none"/>
<path class="st-ink" stroke-width="0.8" d="M18.48 133H21.52"/>
<rect class="fi-surface st-ink" x="16.58" y="136.42" width="6.84" height="3.42" rx="0.38" stroke-width="0.8"/></g><text x="33" y="132.5" class="m fi-ink" font-size="6.2" text-anchor="start" font-weight="700">GECEKODU</text><text x="33" y="140" class="m fi-faint" font-size="4.3" text-anchor="start">19:00 → 07:00</text></g>
<g transform="translate(208 146)"><path class="d s" d="M-11 0.6a11 2.4 0 0 0 22 0"/><path class="d f" d="M-11 0.6a11 2 0 0 1 22 0"/>
<path class="fi-alert" d="M-5.2 -16Q-4.8 -10.5 -2.7 -8Q-4.9 -4.8 -3.6 -1H3.6Q4.9 -4.8 2.7 -8Q4.8 -10.5 5.2 -16Z" fill-opacity=".5" stroke="none"/>
<path stroke-width="1.5" d="M-6 -19Q-5.4 -11 -2.8 -8Q-5.6 -4.6 -3.8 -0.6H3.8Q5.6 -4.6 2.8 -8Q5.4 -11 6 -19"/>
<path class="d f" d="M2 -23L-1 -9"/><g class="d s"><path class="puff" d="M-2.5 -22q-2.5-3 0-6t0-6"/><path class="puff d2" d="M3 -23q-2.5-3 0-6t0-6"/></g></g></svg>`

const shortLinkScene410 = `<svg class="scene" viewBox="0 0 240 180" aria-hidden="true" focusable="false"><path class="s" d="M10 150H230"/>
<rect x="12" y="40" width="132" height="76" rx="6" class="fill-s"/>
<path class="d s" d="M24 52.5a3.4 3.4 0 0 1 4.8 0l1.6 1.6a3.4 3.4 0 0 1 0 4.8M27.4 60.5a3.4 3.4 0 0 1 -4.8 0l-1.6 -1.6a3.4 3.4 0 0 1 0 -4.8"/>
<text x="38" y="58" class="m fi-ink" font-size="5.6" text-anchor="start">YıldızJam kayıt bağlantısı</text>
<text x="22" y="74" class="m fi-faint" font-size="5" text-anchor="start">jam sayacı: 00:00:00</text><rect x="22" y="79" width="112" height="7" rx="3.5" class="d"/>
<g class="stamp"><g transform="rotate(-7 78 102)"><rect x="34" y="92" width="88" height="20" rx="3" class="fill-s st-alert" stroke-width="1.6"/><text x="78" y="105.5" class="m fi-alert" font-size="7.4" text-anchor="middle" font-weight="700">SÜRESİ DOLDU</text></g></g>
<g class="spin selfpivot"><path class="d st-brand" d="M174 78A10 10 0 1 1 167 68.5"/><path class="d st-brand" d="M165 64l3 4.5-5 1.5"/></g>
<rect x="190" y="60" width="40" height="34" rx="5" class="fill-s st-brand" stroke-width="1.4"/>
<text x="210" y="74" class="m fi-brand" font-size="6" text-anchor="middle" font-weight="700">güncel</text><rect class="fi-brand" x="196" y="81" width="28" height="5" rx="2.5" stroke="none"/>
<text x="168" y="106" class="m fi-brand" font-size="4.8" text-anchor="middle">ana sayfa</text></svg>`

var (
	shortLinkPageCSP = "default-src 'none'; style-src 'sha256-" + shortLinkStyleHash() + "'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
	shortLinkPage404 = shortLinkPage("404", "Bu sayfa yok",
		"GECEKODU'nda sabaha karşı yazılmış, sonra kaybolmuş; adresi bir kontrol et.", shortLinkScene404)
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

// ShortLinkRoot is skyl.app/ with no alias (/v1/go and /v1/go/): the club
// site, for a person and a program alike.
func ShortLinkRoot(c fiber.Ctx) error {
	c.Set(fiber.HeaderCacheControl, "no-store")
	c.Set(fiber.HeaderLocation, shortLinkHome)
	return c.SendStatus(fiber.StatusFound)
}
