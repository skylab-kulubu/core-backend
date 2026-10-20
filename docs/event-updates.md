# Event and Season updates

ADR 0066 addendum (2026-10-19), ticket 13. `PUT` and `PATCH` on `/v1/events/{id}` and `/v1/seasons/{id}` are the same partial update: **a field the body does not send keeps its value.** A client that sends the whole object back gets what it got before; a client that sends one field changes that field alone.

## Event: `PUT`/`PATCH /v1/events/{id}`

| Body | Result |
|---|---|
| key left out | the stored value stays |
| a value | written |
| `null` (or `""` for text) | cleared, for a field that may be empty |

| Field | Cleared by `null` / `""` to |
|---|---|
| `description`, `formUrl`, `formAlias`, `linkedin`, `prizeInfo` | empty |
| `extraFormUrls`, `doorStaffIds` (also `[]`) | none |
| `capacity` | `0` |
| `startDate`, `endDate`, `coverImageId`, `attendanceRatio` | none |
| `attendanceRule` | `none` |
| `name`, `location`, `ownerTeam` | may not be cleared: `400` |
| `ranked` | `null` is `400` (send `false`) |

- `cancelReason` edits a cancelled Event's public reason; on any other Event it is ignored ([event-cancel.md](event-cancel.md)).
- `id`, `status` and `active` in the body are not read: the status changes only through its endpoints (`publish`, `unpublish`, …) and `active` is derived.
- A value of the wrong type is `400`; so is `attendanceRule: "ratio"` without a ratio (sent or stored). Rights are checked before the body: who may not edit the Event gets `403` whatever it sends.
- The season is not in the body: `POST /v1/seasons/{id}/events/{eventId}` and `DELETE /v1/events/{id}/season` set it.
- `doorStaffIds` is written only by who may assign door staff; anyone else's is ignored, as before.
- The form short links are bound again only when the body sends `formUrl`, `formAlias`, `extraFormUrls` or `name` (the name labels them). A body without them leaves the links alone, so it cannot take back a link another Event has taken over.

## Season: `PUT`/`PATCH /v1/seasons/{id}`

The same rule for `name` (may not be cleared: `400`), `startDate` and `endDate` (`null` clears) and `active`. A Season's `active` is its own flag, set by hand ("the current season"), not derived: left out it stays, `null` is `400`.

## Clients

- sky-app sends every field it edits and the ones it keeps from the read Event (`ranked`, `prizeInfo`, the attendance rule, the cover) on `PUT`: nothing changes for it. The note for Mobile Lab: "a field the body leaves out is kept".
- The panel's forms are reviewed against this rule in ticket 17 (core-frontend).
