# An Event's place

An Event has three fields for its place (bilet PDF v5, 2026-11-22). The
ticket's "Etkinlik yeri" section and its directions link read them; the
panel sets them.

| Field | What it is | Required |
|---|---|---|
| `location` | The place's name, as people say it: "YTÜ Davutpaşa, EEF Konferans Salonu", "Online". It is on the ticket, in the mails and in the change notice ([event-updated-notification.md](event-updated-notification.md)). | yes, as before |
| `locationLat`, `locationLng` | The place's point on the map, WGS 84 degrees, as a map gives it (the panel's map picker). Both or neither. | no |
| `locationAddress` | The street address, free text up to 300 characters ("Davutpaşa Cd. No:127, Esenler/İstanbul"). | no |

Migration `20261122150000_event_location_point`: `events.location_lat` and
`events.location_lng` (`double precision`, null) and
`events.location_address` (`text`, default `''`), under the check
`events_location_point_check`. It runs by itself at startup; the release
before reads none of the columns and keeps working on the new schema.

## Rules

- **Both or neither.** `locationLat` without `locationLng` (or the other way
  round) is `400` with `code: "event_location_invalid"`, on `POST`, `PUT`
  and `PATCH` alike; so is clearing one of them alone. `null` for both
  clears the point.
- **On the globe.** Latitude −90…90, longitude −180…180. `0, 0` (a point a
  form sends when it lost its value) is refused.
- **Six decimals.** Core rounds the point to six decimals (about 11 cm) and
  answers it so: the panel's map may send more.
- **Address.** Trimmed; at most 300 characters; no control characters but
  line breaks. `null` or `""` clears it.
- The database checks the same (both or neither, the ranges, 0, 0, the
  length) for any writer.

`PUT` and `PATCH` keep what the body leaves out ([event-updates.md](event-updates.md)):
a client that sends neither field keeps the point and the address.

## Where they go

- `GET /v1/events`, `GET /v1/events/{id}` and every Event answer carry the
  three fields; `locationLat`, `locationLng` and `locationAddress` are left
  out while empty. An Event's place is public: the club site's event page
  may show them.
- A copy takes the point and the address with the name
  ([event-copy.md](event-copy.md)).
- The change notice (`event.updated`) still looks at `location` only:
  moving the pin within the same place tells nobody.
- The ticket's directions link is a Google Maps search for the point, or
  for the name while there is no point (bilet PDF v5).

## Places used before

`GET /v1/events/places?ownerTeam=WEBLAB&limit=20` lists the places Events
were held at, for the panel's place field (Yusuf, 2026-11-22: offered from
past Events, no separate list of saved places):

```json
[{"name": "YTÜ Davutpaşa, EEF Konferans Salonu", "address": "Davutpaşa Cd. No:127, Esenler",
  "lat": 41.025376, "lng": 28.889648, "lastUsedAt": "2026-10-14T16:00:00Z", "uses": 2}]
```

- One row per name and point (`location`, `locationLat`, `locationLng`),
  with the address of its latest Event; the latest used first (an Event's
  start, else when it was made). `address` is left out while empty.
- Only current Events with a point; archived ones and places with no point
  are left out.
- `ownerTeam`: that team's Events, drafts included, for who may create
  Events in it. Without it: every team's Events but drafts, for who holds
  `event:manage`. Anyone else `403`, signed out `401`.
- `limit`: 1–50, 20 by default; another value is `400`.
- The panel filters the list as the organizer types and, on a choice,
  fills `location`, `locationAddress`, `locationLat` and `locationLng`
  together; the map's pin goes there.
