# The page for a short link that does not lead anywhere

A `skyl.app` link travels on posters and QR codes, so the person who opens an
unknown or disabled one is usually in a browser. Before this page they saw
the problem JSON (`{"detail":"Not Found",...}`); RFC 9457 bodies are for
programs.

## Who gets what

`GET /v1/go/{alias}` and `GET /v1/go/{alias}/{channel}` look at `Accept` only
when the alias leads nowhere:

| Alias | Caller prefers `text/html` (a browser) | Anyone else |
|---|---|---|
| never existed | `404`, page "Böyle bir adres yok" | `404` problem JSON, as before |
| its link is disabled, or it is the old spelling of a disabled link | `410`, page "Bağlantı eskimiş" | `404` problem JSON, as before |

"Prefers" is `Accept` negotiation over `application/json`,
`application/problem+json` and `text/html`, with a tie going to JSON: no
`Accept`, `*/*` (curl), `application/json` (sky-app) and
`application/json, text/html` all get the JSON. A live link redirects as
before, whatever `Accept` says. Both miss answers carry `Vary: Accept`.

`skyl.app/` with no alias (`GET /v1/go`, and `/v1/go/` with it) is `302` to
`https://yildizskylab.com` with `Cache-Control: no-store`, for everyone.

A disabled link is one that was deleted in the panel (`disabled_at`); the
store answers `AliasDisabled` with the exact spelling, as the redirect does,
so a case variant of a disabled alias is an unknown one. A link whose row is
gone for good is an unknown one too.

## The page

One self-contained HTML file, the same bytes for every request (nothing from
the request is written into it): `lang="tr"`, system fonts, the scene as
inline SVG (`aria-hidden`), dark and light by `prefers-color-scheme`, and the
animation stops under `prefers-reduced-motion`. The button goes to
`https://yildizskylab.com`.

Headers: `Cache-Control: no-store`, `X-Content-Type-Options: nosniff`,
`Referrer-Policy: no-referrer` and
`Content-Security-Policy: default-src 'none'; style-src 'sha256-…'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'`.
The hash is the page's one `<style>` element, computed at start-up; there is
no script and no `style` attribute. The scene's colours are classes in that
`<style>`, not `var(--…)` in SVG presentation attributes (`fill=`,
`stroke=`), which Safari does not resolve.

Scenes and copy come from the error page work (`notes/hata-sayfalari` in the
hub): scene 404-C and 410-B of prototype v5, copy v4 as approved. When the
skylcn `ErrorScene` is published the page can be checked against it; it does
not depend on it.
