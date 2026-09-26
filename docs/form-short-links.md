# Form short links

**Every form has at most one short link in Core, and the forms service and the
event panel show the same one.** A link belongs to the form it points at, not
to the person who created it, so every collaborator sees the same alias, the
same QR code and the same click counts.

A row in `urls` is bound to a form through `form_id`. A unique index keeps one
active row per form. When an event points at the form, the row also carries
`event_id`, and the event decides the alias.

## Who manages the alias

| Link | Alias changes through | Forms shows it as |
|---|---|---|
| Form-managed (`event_id` empty) | `PATCH /v1/urls/forms/{formId}`, called by the forms service for owners and editors | editable |
| Event-managed (`event_id` set) | saving the event with a new `formAlias` or extra-form alias | read-only, labelled "Etkinlik linki" |

Bound links are never renamed or disabled through the generic
`/v1/urls/{id}` endpoints; those answer `409` with code `managed` so two places
cannot fight over one alias. URL moderators can still disable one.

## Endpoints

Form-link endpoints need the `url:forms` role (the forms service account) or
`url:moderator`. The forms service checks form roles itself before it calls.

| Method and path | Body or query | Result |
|---|---|---|
| `GET /v1/urls/forms/{formId}` | | the bound link, `404` when the form has none |
| `PUT /v1/urls/forms/{formId}` | `{ url, label, alias, actorId }` | the bound link, created under the readable `alias` if missing; **idempotent** |
| `PATCH /v1/urls/forms/{formId}` | `{ alias, suggestion }` | renamed link; an empty `alias` asks for the readable default built from `suggestion`; `403` with code `event_managed` on an event link, `409` when taken |
| `GET /v1/urls/forms/{formId}/stats` | | `{ since, total, sources: [{ source, count }] }` for the last 90 days |
| `GET /v1/urls/availability` | `?alias=` | `{ alias, available, reason }`, reason is `invalid`, `reserved` or `taken` |

A new link gets a **readable default name** (ADR 0033): Forms sends the form
title and year as `alias` (for example `yaz-kampi-basvuru-2026`). When that
name is taken Core numbers it (`-2` up to `-9`) and only falls back to a
random alias when every numbered variant is taken. An explicit `alias` on
`PATCH` is never numbered: the person typed that name, so a taken one answers
`409` instead.

`url` must contain the form id; Core refuses a target that points at another
form. `actorId` becomes `created_by` when that account exists in Core.
Otherwise the service account stands in, because `created_by` only accepts
active accounts.

Stats count every link that points at the form, bound or not, so an old
personal link to the form still shows up in the form's channels.

## Channels and QR codes

A shared link carries its channel either as a query or as a short suffix.
Both are recorded as the same `utm_source`:

| Suffix | Recorded as |
|---|---|
| `skyl.app/{alias}/ig` | `instagram` |
| `skyl.app/{alias}/wa` | `whatsapp` |
| `skyl.app/{alias}/li` | `linkedin` |
| `skyl.app/{alias}/mail` | `email` |
| `skyl.app/{alias}/web` | `website` |

The suffix wins over a `utm_source` in the query. An unknown suffix redirects
without a tag, so a mistyped printed link still reaches the form.

`GET /v1/go/{alias}/qr` encodes the `utm_*` tags it is called with, so a poster
printed from `?utm_source=qr&logo=1` counts scans apart from clicks.
`format=svg` returns a vector code for print.

## Aliases

- **Pattern:** a letter or digit, then letters, digits, `-` and `_`, up to 64
  characters.
- **Reserved:** `v1`, `health`, `go`, `urls`, `api`, `docs` and `c`. `c` is
  the certificate page on skyl.app, so `skyl.app/c/ig` could never reach a
  link called `c`.
- **Case-insensitive uniqueness:** `Yaz-Kampi` is taken once `yaz-kampi`
  exists. The redirect itself still matches the alias exactly, so aliases that
  already differ only by case keep resolving to their own links.
- **Retired aliases keep working:** renaming a link retires its old alias. The
  old address keeps redirecting to the same link, so printed QR codes and old
  posts survive the rename, and nobody can ever claim the old alias for
  another page. A link may take its own old alias back.

## Events

Saving, archiving and restoring an event syncs the links of the forms it names
in `formUrl` and `extraFormUrls`:

1. If the form's link already has the alias the event names, it becomes
   event-managed.
2. Otherwise, if a link with that alias points at the form (the panel created
   it before saving the event), that link takes over and the form's previous
   link is released. A released link keeps redirecting as an ordinary link.
3. Otherwise Core creates the alias for the form and takes it over the same
   way.
4. Forms the event no longer names, and every form of an archived event, go
   back to being form-managed.

Syncing never fails the event write. An alias that points at another page, or
that is invalid or retired, is logged and the form keeps its own link.

> **For the event panel:** nothing has to change for the current flow to
> work; Core binds the alias the panel already saves. An alias change never
> breaks the old address, whichever side makes it: the event releases the old
> row, and a rename from Forms retires the old alias, which keeps redirecting.

## Configuration

- Give the forms service client the `url:forms` role.
- The skyl.app proxy already forwards every path to `/v1/go/…`, so
  `/{alias}/ig` reaches the channel route without a proxy change.
