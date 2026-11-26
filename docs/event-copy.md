# Copying an Event

ADR 0066 and its 2026-10-19 addendum (e-skylab), ticket 06. A copy takes the source's **structure** into a new **draft**; people and history stay behind, and so do the forms and their short links (a form has one short link, and every Event that names it writes Tickets for its answers: the panel offers "Geçen seferin formundan yeni form" on the copy instead, Skyforms' `fromForm`).

## `POST /v1/events/{id}/copy`

```json
{
  "id": "<UUID the client makes>",
  "name": "Gecekodu 2027",
  "startDate": "2027-03-14T19:00:00+03:00",
  "includeDoorStaff": false,
  "includeCompetitions": false,
  "includeExternalRegistration": false,
  "includeAttendeeNote": false,
  "includeCertificateTemplate": true,
  "ownerTeam": "ALGOLAB"
}
```

`id`, `name` (trimmed) and `startDate` are required (`400` otherwise). `ownerTeam` is optional: none (or the source's own) keeps the source's Owner team. `includeCertificateTemplate` is optional and on by default: left out or `true` copies the certificate template, `false` leaves it out (named `certificateTemplate` in `skipped` when the source has one).

| Answer | When |
|---|---|
| `201 {"event": Event, "skipped": [...]}` | the copy was made; `skipped` names the parts left out (below), `[]` when none |
| `200 {"event": Event, "skipped": []}` | the same `id` from the same source into the same Owner team made the copy already (a retried request): nothing is written again |
| `409` | the `id` is another Event's, the same copy in another Owner team, or a copy that was deleted since |
| `404` | the source does not exist, is archived, or is a draft the caller may not see |
| `403` | the caller sees the source but may not create Events in the copy's Owner team (a team member, DK, another team's Leader); into another team, also when the caller may not edit the source |

Who: whoever may see the source (a draft: who may edit it, ADR 0066) and create Events in its Owner team (`authz.Create`, the team authority handover included). Any source status is copied: draft, published, cancelled or ended.

**Into another Owner team** (ticket 16, ADR 0066 addendum 2026-10-19): `ownerTeam` names it. The caller must be able to edit the source (`authz.Update`: who only reads it, DK or a signed-in member, may not) and create Events in the target (`authz.Create`); either missing is `403`. Every part is then decided for the target team: the cover by the Team media library (the source's team uses it, so it is left out, `cover`), the template binding by the binding rule (a template of another team is left out), door staff by `authz.Assign` and Competitions by the right to manage them in the target. The Season rule is the same. Which teams the caller may copy into is in `GET /v1/users/me/capabilities` (`teams[].can["event.create"]`, and `otherTeams.can["event.create"]` for every other team).

## What is copied

| | Copied | |
|---|---|---|
| Name | the new one | |
| Description, location, capacity, `ranked`, prize text | yes | |
| The place's point and address (`locationLat`, `locationLng`, `locationAddress`; [event-location.md](event-location.md)) | yes | |
| Owner team | the source's, or `ownerTeam` | see **Into another Owner team** |
| Start | the new one | |
| End, days, Sessions, applying deadline | moved | see **Dates** |
| Season | the current Season the new start falls in, the narrowest when several do (a Season without an end runs on; one without a start covers nothing); none when none | not the source's: a copy is usually for the next term |
| Cover | yes, the same Media | left out (`cover`) when the Media's rules or the Team media library refuse it |
| Certificate rule (`attendanceRule`, ratio) | yes | |
| Event template (the source's own certificate template; [certificate-event-templates.md](certificate-event-templates.md#copy-and-team-change)) | yes, as a new, independent Event template of the copy: the copy's Owner team, the name "<copy's name> sertifikası", the same draft; when it was published, its latest version becomes the copy's version 1 (the same layout, asset copies and checksum; nothing is rendered again), and the copy is bound to it when the source was bound to its own. A draft-only template is copied and binds nothing. The design's link (Canva or Figma `sourceRef`, `sourceEditUrl`) comes along only for who may read the source template (the template rule of its Owner team), else both are empty | left out (`certificateTemplate`) with `includeCertificateTemplate: false`, or when the database refuses its Media (one deleted since: the draft's, or the published version's); the Event copy goes on |
| Team or club template binding (`scope=event`) | yes | left out (`certificateTemplate`) with `includeCertificateTemplate: false`, when the caller may not bind templates for the team (`certificate:binding:manage` or the team's Leader), or the template is archived or another team's |
| Days, Sessions | the current ones | archived ones are not; a Session is never cancelled in the copy, and has no feedback form (its `feedbackAutoSend` comes along); Sessions get new ids, so old Session QR codes do not check anyone in to the copy |
| Door staff | only with `includeDoorStaff`, and only those whose accounts are active | left out (`doorStaff`) when the caller may not assign them (`event:manage`, or a Leader of the copy's Owner team: [co-organizers.md](co-organizers.md)) |
| Competitions and their stages | only with `includeCompetitions`; the current ones, each a `draft` with its name, description and entrant mode; stages with position, name, strategy, config and config version, dates moved | left out (`competitions`) when the caller may not manage Competitions in the team; never copied: entrants, members, submissions, adjustments, results, cancel reasons, archived Competitions |
| External registration link (`externalRegistrationUrl`; [attendee-import.md](attendee-import.md)) | only with `includeExternalRegistration` (the panel asks; an older client sends neither and copies neither) | |
| Attendee note | only with `includeAttendeeNote` | left out (`attendeeNote`) when the caller may not read it on the source (who may edit the Event, or read its Tickets) |
| The door's mail switches (`attendanceWelcomeMail`, `attendanceRecordedMail`) and entry mode | yes | |
| "Otomatik bilet gönder" (`formResponsesAutoTicket`; [form-respondents.md](form-respondents.md)) | yes | |
| Theme and ticket colour (`theme`, `ticketAccentColor`; [event-theme.md](event-theme.md)) | yes | |
| Ticket image ([ticket-image.md](ticket-image.md)) | a cover region when the copy has the cover it was drawn on; an image of its own (an "Ayrı görsel") with its region | the image is left out (`ticketImage`) when the Team media library refuses it in the copy's Owner team (another team's image), or the database refuses its link; a cover region without its cover is not copied (the band is the copy's cover's middle) |
| LinkedIn link, gallery, files, videos, mail list | no | |
| Application and extra forms, their short links | no | |
| Feedback mode (`feedbackMode`), `feedbackAutoSend` | yes | the feedback form and its alias are not copied (one place per form; [event-feedback.md](event-feedback.md)) |
| Applying closed by hand | no: the copy's applying is open | |
| Tickets, check-ins, certificates, batches, jobs, attendance finalization, Competitors | never | |
| Status | always `draft` (publish it with `POST …/publish`) | |
| `copiedFromEventId` | the source | |

A part is named in `skipped` only when the source has it: `doorStaff`, `competitions` and `attendeeNote` only when asked for; `ticketImage` only for an image of the source's own.

## Dates

`startDate` replaces the source's **anchor**: its start date, else its first current day's start, else its first current Session's start. Every other date moves by the same distance on the Europe/Istanbul wall clock, so durations and gaps stay. A source with no date at all moves nothing: the copy gets the new start, and its undated days and Sessions stay undated. Day names ("Cumartesi") are copied as they are.

## One transaction

The Event, its door staff, days, Sessions, template binding, Competitions and cover link are written in one transaction: a failure halfway leaves nothing. The cover is linked in a savepoint, so that a refusal by the link's trigger leaves the cover out (`cover`) instead of the copy. No form link is synced: the copy has no form. The source is read `FOR SHARE`, so it is not archived while it is copied.
