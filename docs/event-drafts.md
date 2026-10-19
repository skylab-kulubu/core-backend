# Event drafts

ADR 0066 and its 2026-10-19 addendum (e-skylab). An Event's `status` is `draft`, `published` or `cancelled`; this page is what a draft means for the API.

## Who sees a draft

`event.VisibleTo`: a draft exists only for whoever may edit the Event (`authz.Update` on its Owner team, the team authority handover included) and for a Privileged reader (DK, `global:read`), who only reads it. The authorizer answers both as `ReadFull` on the Event. Everyone else (signed out, a Member, the door staff, another team's Leader, a product's service account) gets `404` on every path below, as if the draft did not exist.

## Reads

| Path | Draft |
|---|---|
| `GET /v1/events`, `?ownerTeam=`, `?active=true` | never listed |
| `GET /v1/events?drafts=include` | adds the drafts the caller may see: an editor their teams', DK every team's; ignored for anyone else and signed out; any other value of `drafts` is `400` |
| `GET /v1/events?lifecycle=all` | the current list and the archive together: a current draft only with `drafts=include`, an archived draft as in `lifecycle=inactive` |
| `GET /v1/events?lifecycle=inactive` (the archive) | an archived draft for who may see it and holds `authz.Delete` (where a deleted draft is restored from) |
| `GET /v1/events/active`, `GET /v1/seasons/{id}/events`, `GET /v1/door/events` | never listed |
| `GET /v1/events/{id}`, `…/days`, `/v1/event-days/{id}`, `…/sessions`, `…/current-session`, `/v1/sessions/{id}`, `/v1/sessions/{id}/qr` | `404` unless the caller may see it |
| `GET /v1/events/{id}/competitions`, `/v1/competitions/{id}` (and every other competition path) | `404` unless the caller may see the Event |
| `GET /v1/events/{id}/tickets`, `…/assignable-users`, `…/door-attendees`, `/v1/sessions/{id}/check-ins` | `404` unless the caller may see it; then the usual rules |
| `GET /v1/competitors/{id}`, `GET /v1/events/{id}/competitors`, `…/competitors/winner` | `404` unless the caller may see the Event |
| `GET /v1/competitors/me`, `/v1/competitors/user/{id}`, `/v1/competitors/team/{team}`, the leaderboards, `?lifecycle=` | leave out a draft's Competitors for who may not see it (their own included) |
| `GET /v1/events/{id}/certificates`, `…/certificates/summary`, `…/certificates/template`, `POST …/certificates/preview`, `GET …/certificate-batches`, `/v1/certificate-batches/{id}`, the Event's template binding | `404` unless the caller may see it; then the usual rules |
| `GET /v1/tickets/{id}`, `GET /v1/tickets/user/{userId}/event/{eventId}` | their holder always reads their own (`status: "draft"`); anyone else `404` unless they may see the Event |
| `GET /v1/tickets?userId=`, `?email=` | leave out a draft's Tickets for who may not see it (the holder's own query aside) |
| `GET /v1/dashboard/summary` | a draft is covered only for who may see it, listed in `eventStats` with `status: "draft"`; it never counts as `upcoming` or `live` (neither does a cancelled Event) |

## Writes

| Path | Who may see the draft | Anyone else |
|---|---|---|
| `POST /v1/events/{id}/applications/me`, `…/applications/guest` | `409`, `code: "event_not_published"` | `404` |
| `POST /v1/events/{id}/applications/users/{userId}` (add a person from the panel) | allowed as on a published Event (DK: `403`) | `404` |
| `POST /v1/forms/{formId}/responses` (Forms' report) | writes the Ticket: the editors linked the form and shared it | |
| `check-in`, `check-in/me`, `check-in/guest`, `check-in/resolve`, `check-in/skypass`, `door-qr` | `403`, `code: "session_closed"`, `reason: "event_not_published"` | `404` |
| `POST /v1/events/{id}/certificates/issue`, `…/finalize`, `…/recompute` | `409`, `code: "event_not_published"` (after the usual rights: DK `403`) | `404` |
| `POST /v1/events/{id}/mail-list` | allowed as on a published Event (DK: `403`) | `404` |
| `POST /v1/competitors`, update, withdraw, reinstate | `400`/`409` `event_not_published` as before | `404` |

A certificate is issued only on a published Event: issuing one by hand on a cancelled Event is `409 event_cancelled`, as finalization already was. The legacy immediate issuance (`LegacyImmediate`) skips an Event that is not published.

Writes on the Event itself (update, delete, days, Sessions, media) keep their rules: they read `authz.Update`/`Create`/`Delete`, never `VisibleTo`, so DK gets `403`.

## Publish and unpublish

- `POST /v1/events/{id}/publish`: draft → published, with `publishedAt` and the publisher. Needs a name, a location and a start date: otherwise `400`, `code: "event_incomplete"`, `missing: ["startDate", …]`. Publishing a published Event answers it unchanged; a cancelled one is `409 event_cancelled`.
- `POST /v1/events/{id}/unpublish`: published → draft, only while the Event has no Ticket (a check-in needs one) and no current Competitor: otherwise `409 event_has_tickets` or `409 event_has_competitors` (a withdrawn Competitor does not hold it). Unpublishing a draft answers it unchanged; a cancelled one is `409 event_cancelled`. `publishedAt` keeps the last publication.
- Both: whoever may edit the Event (`authz.Update`); DK `403`; anyone who may not see the Event `404`. Each is one `UPDATE … WHERE status = <expected>` (the publish also checks the three fields there), so two at once change the row once; the answer is the Event (`200`). The legacy `events.active` column is written with the status, as on create, for a rollback.
