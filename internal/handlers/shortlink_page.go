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
:root{--bg:#171717;--fg:#f5f5f5;--border:rgb(255 255 255/14%);--ink:#e5e5e5;--soft:#a3a3a3;--faint:#737373;--brand:#e0c8e5;--on-brand:#0a0a0a;--alert:#f87171;--surface:var(--bg);color-scheme:dark}
@media (prefers-color-scheme: light){:root{--bg:#f6f4f1;--fg:#1c1a18;--border:rgb(0 0 0/14%);--ink:#2b2825;--soft:#6b665f;--faint:#8a847d;--brand:#8f5e98;--on-brand:#fff;--alert:#b91c1c;color-scheme:light}}
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
.code{margin:12px 0 0;font:400 12px/1 ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;letter-spacing:.14em;color:var(--soft)}
h1{margin:10px 0 0;font-size:22px;line-height:1.25;font-weight:600;letter-spacing:-.01em;text-wrap:balance}
p.line{margin:8px 0 0;max-width:34ch;font-size:15px;line-height:1.5;color:var(--soft);text-wrap:pretty}
.btn{display:inline-flex;align-items:center;min-height:44px;margin-top:18px;padding:0 16px;font-family:inherit;font-size:14px;font-weight:500;line-height:1;color:var(--fg);text-decoration:none;border:1px solid var(--border);background:rgb(127 127 127/8%);border-radius:8px}
.btn:hover{background:rgb(127 127 127/16%)}
.btn:focus-visible{outline:2px solid var(--brand);outline-offset:2px}
.fall{animation:fall 3.6s cubic-bezier(.5,0,.8,.6) infinite}
@keyframes fall{0%,25%{transform:translate(0,0) rotate(0);opacity:1}100%{transform:translate(10px,70px) rotate(10deg);opacity:0}}
.snip{animation:snip 3.6s ease-in-out infinite;transform-box:view-box}
@keyframes snip{0%,18%,30%,100%{transform:scaleX(1)}24%{transform:scaleX(.55)}}
.sig1,.sig2,.sig3{animation:sig 2.4s steps(1) infinite}
.sig2{animation-delay:.4s}
.sig3{animation-delay:.8s}
@keyframes sig{0%,49%{opacity:1}50%,100%{opacity:.15}}
.bob{animation:bob 2.6s ease-in-out infinite}
@keyframes bob{50%{transform:translateY(-3px)}}
.drop{animation:drop 2.8s cubic-bezier(.3,0,.2,1) infinite}
@keyframes drop{0%{transform:translateY(-14px);opacity:0}25%{transform:translateY(0);opacity:1}32%{transform:translateY(-3px)}38%,100%{transform:translateY(0)}}
@media (prefers-reduced-motion: reduce){.scene *{animation:none!important}}
`

// The two scenes come from the error page prototype v5 (404-C and 410-B);
// the copy is v4's, as approved.
const shortLinkScene404 = `<svg class="scene" viewBox="0 0 240 180" aria-hidden="true" focusable="false"><path class="s" d="M10 150H230"/>
<rect x="14" y="44" width="122" height="20" rx="10" class="fill-s"/>
<path class="d s" d="M24 50.5a3 3 0 0 1 4.2 0l1.4 1.4a3 3 0 0 1 0 4.2M27 57.5a3 3 0 0 1 -4.2 0l-1.4 -1.4a3 3 0 0 1 0 -4.2"/>
<text x="36" y="57" class="m" font-size="6.2" fill="var(--ink)" text-anchor="start">yildizskylab.com/etk</text>
<path stroke="var(--alert)" class="d" stroke-dasharray="2 2" d="M138 40V68"/>
<g class="fall"><g transform="rotate(18 168 54)"><path class="fill-s" d="M142 44H178a10 10 0 0 1 0 20H142Z"/><text x="146" y="57" class="m" font-size="6.2" fill="var(--faint)" text-anchor="start">inlik/43</text></g></g>
<g transform="translate(138 82)"><g class="snip">
<path class="fill-s" d="M0 0L-6 -26L0 -6Z"/><path class="fill-s" d="M0 0L6 -26L0 -6Z"/>
<circle cx="-6" cy="10" r="6" class="fill-s"/><circle cx="6" cy="10" r="6" class="fill-s"/><path class="d" d="M-3 5L0 0L3 5"/></g></g>
<rect x="22" y="100" width="96" height="34" rx="8" class="fill-s"/><path class="fill-s" d="M34 134l-4 9 12-9"/>
<text x="30" y="114" class="m" font-size="5.6" fill="var(--ink)" text-anchor="start">bağlantının sonu</text><text x="30" y="124" class="m" font-size="5.6" fill="var(--ink)" text-anchor="start">kesilmiş, yeniden at?</text>
<g class="sig1"><circle cx="186" cy="124" r="2" fill="var(--soft)" stroke="none"/></g><g class="sig2"><circle cx="194" cy="124" r="2" fill="var(--soft)" stroke="none"/></g><g class="sig3"><circle cx="202" cy="124" r="2" fill="var(--soft)" stroke="none"/></g>
<rect x="174" y="112" width="40" height="24" rx="8" class="d f"/></svg>`

const shortLinkScene410 = `<svg class="scene" viewBox="0 0 240 180" aria-hidden="true" focusable="false"><path class="s" d="M10 150H230"/>
<g transform="rotate(-4 70 96)"><rect x="24" y="70" width="92" height="60" rx="2" class="fill-s"/><path class="d" d="M24 71L70 106L116 71"/></g>
<path class="d f" d="M24 66L52 66M24 66L24 92M24 66L44 84M24 66L50 75M24 66L35 90M32 66Q30 72 24 74M40 66Q37 78 24 82M48 66Q44 84 24 89"/>
<path class="d" d="M86 20V60"/><g class="bob"><circle cx="86" cy="62" r="3.4" fill="var(--ink)" stroke="none"/><path class="d" d="M80 59l3 2M80 65l3-2M92 59l-3 2M92 65l-3-2"/></g>
<text x="70" y="146" class="m" font-size="5.2" fill="var(--faint)" text-anchor="middle">bekleme: çok</text>
<circle cx="40" cy="138" r=".9" fill="var(--faint)" stroke="none"/><circle cx="104" cy="136" r="1.1" fill="var(--faint)" stroke="none"/><circle cx="118" cy="128" r=".8" fill="var(--faint)" stroke="none"/>
<g class="drop"><rect x="156" y="82" width="60" height="40" rx="2" class="fill-s"/><path class="d" d="M156 83L186 106L216 83"/><rect x="196" y="74" width="26" height="12" rx="6" fill="var(--brand)" stroke="none"/><text x="209" y="82.4" class="m" font-size="5.6" fill="var(--on-brand)" text-anchor="middle" font-weight="700">yeni</text></g></svg>`

var (
	shortLinkPageCSP = "default-src 'none'; style-src 'sha256-" + shortLinkStyleHash() + "'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
	shortLinkPage404 = shortLinkPage("404", "Böyle bir adres yok",
		"Bağlantı kopyalanırken sonu kesilmiş olabilir; gönderenden yeniden iste.", shortLinkScene404)
	shortLinkPage410 = shortLinkPage("410", "Bağlantı eskimiş",
		"Bu bağlantı fazla beklemiş, artık geçerli değil; işlemi baştan başlat, yenisi gelsin.", shortLinkScene410)
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
