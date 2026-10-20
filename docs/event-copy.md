# Copying an Event

ADR 0066 and its 2026-10-19 addendum (e-skylab), ticket 06. A copy takes the source's **structure** into a new **draft**; people and history stay behind, and so do the forms and their short links (a form has one short link, and every Event that names it writes Tickets for its answers: the panel offers "Geçen seferin formundan yeni form" on the copy instead, Skyforms' `fromForm`).

## `POST /v1/events/{id}/copy`

```json
{
  "id": "<UUID the client makes>",
  "name": "Gecekodu 2027",
  "startDate": "2027-03-14T19:00:00+03:00",
  "includeDoorStaff": false,
  "includeCompetitions": false
}
```

`id`, `name` (trimmed) and `startDate` are required (`400` otherwise).

| Answer | When |
|---|---|
| `201 {"event": Event, "skipped": [...]}` | the copy was made; `skipped` names the parts left out (below), `[]` when none |
| `200 {"event": Event, "skipped": []}` | the same `id` from the same source made the copy already (a retried request): nothing is written again |
| `409` | the `id` is another Event's (or a copy that was deleted since) |
| `404` | the source does not exist, is archived, or is a draft the caller may not see |
| `403` | the caller sees the source but may not create Events in its Owner team (a team member, DK, another team's Leader) |

Who: whoever may see the source (a draft: who may edit it, ADR 0066) and create Events in its Owner team (`authz.Create`, the team authority handover included). Any source status is copied: draft, published, cancelled or ended.

## What is copied

| | Copied | |
|---|---|---|
| Name | the new one | |
| Description, location, capacity, `ranked`, prize text | yes | |
| Owner team | the source's | another team: ticket 16 |
| Start | the new one | |
| End, days, Sessions, applying deadline | moved | see **Dates** |
| Season | the current Season the new start falls in, the narrowest when several do (a Season without an end runs on; one without a start covers nothing); none when none | not the source's: a copy is usually for the next term |
| Cover | yes, the same Media | left out (`cover`) when the Media's rules or the Team media library refuse it |
| Certificate rule (`attendanceRule`, ratio) | yes | |
| Event template binding (`scope=event`) | yes | left out (`certificateTemplate`) when the caller may not bind templates for the team (`certificate:binding:manage` or the team's Leader) or the template is archived or another team's |
| Days, Sessions | the current ones | archived ones are not; a Session is never cancelled in the copy; Sessions get new ids, so old Session QR codes do not check anyone in to the copy |
| Door staff | only with `includeDoorStaff`, and only those whose accounts are active | left out (`doorStaff`) when the caller may not assign them (`event:manage`) |
| Competitions and their stages | only with `includeCompetitions`; the current ones, each a `draft` with its name, description and entrant mode; stages with position, name, strategy, config and config version, dates moved | left out (`competitions`) when the caller may not manage Competitions in the team; never copied: entrants, members, submissions, adjustments, results, cancel reasons, archived Competitions |
| LinkedIn link, gallery, files, videos, mail list | no | |
| Application and extra forms, their short links | no | |
| Applying closed by hand | no: the copy's applying is open | |
| Tickets, check-ins, certificates, batches, jobs, attendance finalization, Competitors | never | |
| Status | always `draft` (publish it with `POST …/publish`) | |
| `copiedFromEventId` | the source | |

A part is named in `skipped` only when the source has it: `doorStaff` and `competitions` only when asked for.

## Dates

`startDate` replaces the source's **anchor**: its start date, else its first current day's start, else its first current Session's start. Every other date moves by the same distance on the Europe/Istanbul wall clock, so durations and gaps stay. A source with no date at all moves nothing: the copy gets the new start, and its undated days and Sessions stay undated. Day names ("Cumartesi") are copied as they are.

## One transaction

The Event, its door staff, days, Sessions, template binding, Competitions and cover link are written in one transaction: a failure halfway leaves nothing. The cover is linked in a savepoint, so that a refusal by the link's trigger leaves the cover out (`cover`) instead of the copy. No form link is synced: the copy has no form. The source is read `FOR SHARE`, so it is not archived while it is copied.
