# Short link statistics

`GET /v1/urls/{id}/stats?days=30` summarises one short link's clicks for the
panel's chart. It counts and says nothing else: no click's address, browser
string, account, referring path or free UTM text leaves core.

| Field | What |
|---|---|
| `since`, `days` | the window: today and the `days - 1` days before it, as calendar days in Europe/Istanbul; `days` is 1 to 90 (the click list's window), 30 when left out |
| `total`, `scans` | clicks in the window; `scans` is the part that came from a QR code |
| `daily` | one `{ day: "YYYY-MM-DD", count }` per day of the window, oldest first, zero included |
| `sources` | `{ source, count, scans }` by channel: a known channel (`shorturl.KnownSources`), `other`, or `""` for an untagged click |
| `referrers` | `{ name, count }` by the referring site's host without `www.`; `""` is a click that named no site; past the first ten, sites are counted together as `other` |
| `devices` | `{ name, count }` by device: `mobile`, `tablet`, `desktop`, `bot` (link previews and crawlers), `unknown` |

Who reads it: a URL moderator any link's, the person who made a link their own
(`url.listMine`). A disabled or unknown link answers 404, `days` outside 1–90
answers 400. The answer is `Cache-Control: no-store`.

The summary is computed from the kept click rows (`url_hits`) at read time; a
link's clicks in 90 days are a few thousand at most. There is no country:
core stores no location for a click.
