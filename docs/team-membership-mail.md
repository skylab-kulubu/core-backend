# Team membership mail

A person added to a team, or removed from one, through core's membership
routes gets the SkyMail template `club.team-membership` ("SKY LAB takım
üyeliğinde değişiklik"):

- `POST /v1/groups/:groupId/members` (admin panel: add a person to a Group)
- `DELETE /v1/groups/:groupId/members/:userId` (remove a person; through a
  parent roster this removes the subgroup they sit in, see CONTEXT.md
  "Source group")

A change made in Keycloak's own console sends nothing: core does not see it.

## Which Groups are teams

`teammail.TeamOf` decides, from the Group's path alone:

| Path | Team? | `TeamName` |
| --- | --- | --- |
| `/UYELER/<area>/<team>` (e.g. `/UYELER/ARGE/WEBLAB`) | yes | the team's `display_name_tr`, else its name: `WEBLAB` |
| `/UYELER/<area>/<team>/LIDERLER` | yes | `WEBLAB · Liderler` |
| `/UYELER/<area>/<team>/KOORDINATORLER` | yes | `ARTLAB · Koordinatörler` |
| a sub-team below a team (e.g. `/UYELER/ARGE/ALGOLAB/AGC`, and its `LIDERLER`) | yes | `AGC`, `AGC · Liderler` |
| `/UYELER` (the club), `/UYELER/<area>` (ARGE, ORGANIZASYON, …, and technical Groups such as `ESKI-EDITORLER`), an area's own `LIDERLER` | no | |
| any Privileged Group and everything under it: `ADMIN`, `YK`, `DK` (`/ADMIN`, `/UYELER/YK/BASKAN`, …) | no | |
| anything outside `/UYELER` | no | |

The team is read from the directory when the mail is sent, so a team renamed
in between is mailed under its new display name; one that is gone is mailed
under the last segment of its path.

## Variables

| Variable | Value |
| --- | --- |
| `TeamName` | as in the table above |
| `Action` | `added` or `removed` (the template renders "eklendi" / "çıkarıldı") |
| `EffectiveAt` | the day of the change in Europe/Istanbul, `dd.MM.yyyy` |
| `LeaderName` | the full name of the person who made the change ("İşlemi yapan"); empty, and the line left out, for a service account or someone core may no longer name |

The mail goes to the person's primary e-mail (Keycloak's `email`), with core's
profile name as the recipient name.

## One mail per change

- **Add:** before the write, core reads the person's direct Groups. Already a
  direct member: Keycloak takes the add again, nothing changed, no mail.
  Joining a team's `LIDERLER` while a member of the team is a change of its
  own and is mailed.
- **Remove:** only when the person is on the roster; the mail names the Group
  they were removed from (the subgroup they sat in).
- A write that fails sends nothing. A Group that is not a team costs no extra
  read.
- Delivery is at least once: a core that stops between SkyMail's `201` and
  the row's deletion sends that mail again after the lease (two minutes).

## Delivery

```
membership write ──▶ Keycloak (204) ──▶ INSERT team_membership_mails ──▶ answer
                                              │
                         worker (wakes at once, polls every 15 s)
                                              │
               read person + team + actor ──▶ POST SkyMail /v1/mail_tasks/single
```

- The queue is `team_membership_mails`: ids, the Group's path, the action and
  the time, never an address or a name. A pass leases the rows it takes
  (`FOR UPDATE SKIP LOCKED`), so several cores share it.
- The insert is the only thing the membership request waits for (at most
  5 s). If it fails, the change still stands: core logs one line without the
  person and counts `skylab_team_membership_mail_queue_errors_total`.
- At send time, the person is skipped — the row deleted, nothing sent — when
  they are erased, being erased (`deletion_pending`), hard-purged, disabled in
  Keycloak or gone (`skipped_inactive`), or have no primary e-mail
  (`skipped_no_email`).
- SkyMail's answer decides the rest: `2xx` deletes the row; `400`/`422` (the
  body itself is refused) drops it as `rejected`; anything else — `404` (the
  template is not seeded yet), `401`/`403`, `429`, `5xx`, no token, no
  connection — retries after 30 s, doubling to 30 min. A change still unsent
  after 72 hours is dropped as `expired`.
- Account erasure (`anonymize_core`) deletes the person's waiting rows and
  removes them as the actor of others.

## Configuration

| Variable | Unset | Empty |
| --- | --- | --- |
| `SKYMAIL_TEAM_MEMBERSHIP_TEMPLATE_KEY` | `club.team-membership` | off |

It needs core's SkyMail client (`KEYCLOAK_URL`, `KEYCLOAK_REALM`,
`KEYCLOAK_CLIENT_ID`, `KEYCLOAK_CLIENT_SECRET`, `SKYMAIL_URL`). Startup says
which state it is in: `team membership mail: on (template …)` or
`team membership mail: off (…)`. Off, the membership routes read nothing for
it. There is no template id fallback.

## Metrics (`/v1/metrics`, internal)

- `skylab_team_membership_mail_enabled` — 1 on, 0 off
- `skylab_team_membership_mail_total{outcome}` — `sent`, `failed` (retried),
  `rejected`, `expired`, `skipped_inactive`, `skipped_no_email`
- `skylab_team_membership_mail_enqueued_total`,
  `skylab_team_membership_mail_queue_errors_total`
- `skylab_team_membership_mail_backlog`,
  `skylab_team_membership_mail_oldest_age_seconds`,
  `skylab_team_membership_mail_last_success_timestamp_seconds`

A growing `oldest_age_seconds` with `failed` rising means SkyMail is refusing:
the `skymail_call_failed` line with `"kind":"team_membership"` carries the
status ([skymail-templates.md](skymail-templates.md)).

## Sending a mail for an earlier change

Nothing is sent for changes made before this mail existed. To send one by
hand, remove the person from the team and add them again in the admin panel:
that is two real changes and sends two mails (removed, then added). To send
only the "added" mail, queue the row directly (production database, `core`):

```sql
INSERT INTO team_membership_mails (id, subject_id, actor_id, group_path, action, occurred_at, next_attempt_at)
VALUES (gen_random_uuid(), '<person id>', '<actor id or NULL>', '/UYELER/<area>/<team>', 'added', '<when>', now());
```

`occurred_at` is the day the mail shows; keep it within 72 hours of now or the
worker drops the row as expired before sending it.
