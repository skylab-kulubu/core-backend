# Event templates

An Event template is a certificate template that belongs to one Event (`certificate_templates.event_id`). It is made from the Event (the Event page or its edit screen), starts as a copy of the template the Event uses, and serves that Event only. Team and club templates are unchanged.

## Model

- `event_id` names the Event; `NULL` is a team or club template. One current (not archived) Event template per Event: the partial unique index `certificate_templates_one_per_event`. Events are archived, never deleted, so the key is `ON DELETE RESTRICT`.
- `owner_team` is the Event's Owner team. Editing the template never changes it (the body's `ownerTeam` is ignored).
- An Event template is bound only to its own Event (`scope=event`, `scopeKey` = its Event). Any other binding is `400 template_event_scoped`.
- Publishing it binds its Event in the same request: a missing Event binding is set, another template's is switched. The version is stored first; if the binding then fails the request fails, the version stays and publishing again binds it.
- Back to the team or club template: clear the Event binding (`DELETE /v1/certificate-template-bindings/event/{eventId}`). The Event template stays as it is and may be published or bound again.
- Which template a certificate takes is unchanged (ADR-0038): the Event binding, the Owner team binding, the club default. A draft is never used; a batch keeps the version it resolved; an issued certificate never changes.
- The template list (`GET /v1/certificate-templates`) leaves Event templates out; `?eventId=` lists that Event's alone.
- An archived or cancelled Event keeps its template untouched.
- Copying an Event copies its Event template as the copy's own, and changing an Event's Owner team moves its template along (below).

## Who may do what

Every decision on an Event template follows its Event: who may edit it (`canEditEvent`, `Update` on its Owner team), with the Owner team's template rule as the fallback (`canReadEventTemplates`, `canWriteEventTemplate`). Co-organizers (ADR-0092) change who may edit an Event. When they come, these places need updating as well, not only `canEditEvent`:

- the fallbacks above, which read the Owner team's certificate workspace and template rule only;
- `fitsEvent` and the Owner team check in `SetBinding`, which offer and accept the Owner team's and the club's templates only (ADR-0092 §5.8 adds the Co-organizer teams');
- the `certificate_template_editor` upload rule, which looks for an Owner team the person edits Events of (an upload names no Event).

| Action | Who |
|---|---|
| Read the Event template, the state, the resolution and the Event preview | Who reads the Event's certificate workspace, or edits the Event. |
| Create, edit, publish the Event template; bind it to its Event | Who edits the Event (team members where the team's rule lets members edit, as GECEKODU), or whom the Owner team's template rule allows (Leader, `certificate:template:manage`, Privileged). |
| Bind the Event to a team or club template, or clear the Event binding | Who edits the Event, or whom the template rule allows to assign (Leader, `certificate:binding:manage`, Privileged). |
| Edit team and club templates | Unchanged: Leader, `certificate:template:manage`, Privileged. |

A certificate asset (`certificate_asset`, the `certificate_template_editor` upload rule) is uploaded by who may create a certificate template or edit an Event for at least one Owner team. `config/media-purposes.json` is unchanged. Issuing, revoking and the rule keep their own permissions.

## API

`GET /v1/events/{eventId}/certificate-template`

```json
{
  "resolution": { "template": {…}, "version": {…}, "source": "ownerTeam" },
  "eventTemplate": { "id": "…", "name": "Gece Kodu 2026 sertifikası", "draftUpdatedAt": "…", "publishedVersion": 2 },
  "choices": [ { "id": "…", "name": "GECEKODU", "ownerTeam": "GECEKODU", "system": false, "version": 3 } ]
}
```

- `resolution` is what a certificate issued now takes, `null` when nothing resolves; its template and version in full only to who may read that template.
- `eventTemplate` is `null` without one; `publishedVersion` is left out while it is a draft only.
- `choices` are the published team and club templates the Event may be bound to (no Owner team, or the Event's): names and versions, never a layout.

`POST /v1/events/{eventId}/certificate-template` with `{"from": "current" | "blank" | "<templateId>"}` (empty is `current`) answers `201` with the template:

- `current`: the published layout of the template the Event uses now, with its source; an Event nothing resolves for starts blank.
- `blank`: an empty A4 landscape page with the participant's name, the Event's name and the verification QR (`sourceKind` `sky`).
- `<templateId>`: one of `choices`, its published layout; any other id is `404`.

The source's reference and edit address (a Canva or Figma design) are copied only for who may read the source template; anyone else gets its layout with neither.

The copy links the same Media as its source (a Media may have several links). Its name is "<Event name> sertifikası". A second one is `409 template_event_exists` with `templateId`.

`GET/PUT /v1/certificate-templates/{id}`, `POST …/preview`, `POST …/publish` and `PUT /v1/certificate-template-bindings` work on Event templates with the rules above.

The mobile contract does not change (`docs/mobile-certificates-v2.md`).

## Copy and team change

**Copy** (`POST /v1/events/{id}/copy`, in the copy's transaction):

- The source's current Event template becomes a new, independent Event template of the copy: a new id, the copy's Owner team, the name "<copy's name> sertifikası" and the same draft. When it was published, its latest version is written as the copy's version 1 with the same layout, asset manifest (the same asset copies), checksum and serving flag; nothing is rendered again. Published by the copier.
- The copy is bound to its own template when the source was bound to its Event template. A draft-only template is copied and binds nothing.
- The design's reference and edit address (a Canva or Figma link) go along only for who may read the source template (the template rule of the source's Owner team), as when an Event template is made from a team template.
- A source bound to a team or club template: as before, the copy is bound to it when the copier may assign in the copy's Owner team and the template fits that team.
- Who creates the copy may make its Event template, so no further permission is asked for the Event template itself.
- Left out and named in `skipped` (`certificateTemplate`): the copier asked to (`includeCertificateTemplate: false`; the default copies), may not bind the team template, the team template does not fit, or the database refuses the Event template's Media (one deleted since it was published). The Event copy goes on.
- Uploaded fonts (later) live in the version's asset copies and come along with them.

Version asset copies (`certificate-template-assets/<version>/<media>`) are never deleted, except a failed publish's own; two versions may share them after a copy. No code deletes a template version either; `TestVersionsAndTheirAssetCopiesAreNeverDeleted` fails when one starts to, which would then need reference counting.

**Team change:** when an Event's Owner team changes (`PUT /v1/events/{id}`), its Event templates take the new Owner team in the same transaction.

