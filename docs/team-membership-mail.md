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
| `/UYELER/<area>/<team>` (e.g. `/UYELER/ARGE/WEBLAB`) | yes | code · Turkish name: `WEBLAB · Web Geliştirme` |
| `/UYELER/<area>/<team>/LIDERLER` | yes | the team's: `WEBLAB · Web Geliştirme` (`Role` = `leader`) |
| `/UYELER/<area>/<team>/KOORDINATORLER` | yes | the team's: `WEBLAB · Web Geliştirme` (`Role` = `coordinator`) |
| a sub-team below a team (e.g. `/UYELER/ARGE/ALGOLAB/AGC`, and its `LIDERLER`) | yes | the sub-team's: `AGC` |
| `/UYELER` (the club), `/UYELER/<area>` (ARGE, ORGANIZASYON, …, and technical Groups such as `ESKI-EDITORLER`), an area's own `LIDERLER` | no | |
| any Privileged Group and everything under it: `ADMIN`, `YK`, `DK` (`/ADMIN`, `/UYELER/YK/BASKAN`, …) | no | |
| anything outside `/UYELER` | no | |

The code is the team Group's name and the Turkish name its `display_name_tr`
(the name the public team list shows); without a Turkish name, or with one
equal to the code, the code alone (Yusuf, 2026-10-09). A leader subgroup is
named by its team; the role travels in `Role`. The team is read from the
directory when the mail is sent, so a team renamed in between is mailed under
its new name; one that is gone is mailed under the last segment of its path.

## Variables

| Variable | Value |
| --- | --- |
| `TeamName` | as in the table above |
| `Action` | `added` or `removed` (the template renders "eklendi" / "çıkarıldı") |
| `Role` | `member` (the team itself), `leader` (`LIDERLER`) or `coordinator` (`KOORDINATORLER`): the template words its text by it, since only leaders and coordinators get management rights |
| `EffectiveAt` | the day of the change in Europe/Istanbul, `dd.MM.yyyy` |
| `LeaderName` | the full name of the person who made the change ("İşlemi yapan"); empty, and the line left out, for a service account or someone core may no longer name |

The mail goes to the person's primary e-mail (Keycloak's `email`), with core's
profile name as the recipient name.

## One mail per change

- **Add:** before the write, core reads the Group (skipped when the route
  names it by a path that is no team) and, for a team, the person's direct
  Groups, together bounded to 3 s. Already a direct member: Keycloak takes the
  add again, nothing changed, no mail. A read that fails or times out lets the
  add go on without a mail, and is logged without the person and counted
  (`skylab_team_membership_mail_precheck_errors_total`).
  Joining a team's `LIDERLER` while a member of the team is a change of its
  own and is mailed.
- **Remove:** only when the person is on the roster; the mail names the Group
  they were removed from (the subgroup they sat in).
- A write that fails sends nothing. A Group that is not a team costs no
  membership read (and no read at all when named by path).
- One row waits per person, Group and action: the same change made again
  before its mail went (a double click on add) is not queued twice
  (`coalesced_total{kind="duplicate"}`).
- Opposite changes cancel out while unsent: a removal made while the add's
  mail still waits unclaimed (not being sent by a pass; one waiting for its
  retry backoff counts) deletes that row and is not queued
  (`coalesced_total{kind="cancelled"}`). So add, remove, add again before
  any mail went sends one "added" mail. A change already being sent is not
  cancelled; the opposite one is queued after it.
- Delivery is at least once: a core that dies between SkyMail's `201` and
  the row's deletion sends that mail again after the lease (two minutes). A
  core that is merely stopping deletes the row first. If SkyMail writes the
  mail and answers `201` but the answer does not reach core before the send
  is cancelled or its 45 s run out, core counts the send as failed (or
  interrupted) and the mail goes a second time. SkyMail's
  `/v1/mail_tasks/single` takes no idempotency key today, so such a repeat
  is a real second mail.

## Delivery

```
membership write ──▶ Keycloak (204) ──▶ INSERT team_membership_mails ──▶ answer
                                              │
                         worker (wakes at once, polls every 15 s)
                                              │
               read person + team + actor ──▶ POST SkyMail /v1/mail_tasks/single
```

- The queue is `team_membership_mails`: ids, the Group's path, the action and
  the time, never an address or a name. A pass takes one row at a time
  (`FOR UPDATE SKIP LOCKED`), leases it for two minutes (one send is bounded
  to 45 s) and stamps `claimed_at`; it deletes or retries the row only while
  `claimed_at` is still its own. A retry releases the claim, so the row
  waits for its backoff unclaimed. Several cores share the queue, and a core
  whose lease ran out cannot touch a row another core took since.
- After the write the membership request waits for the insert (at most 5 s),
  besides the reads above. If it fails, the change still stands: core logs
  one line without the person and counts
  `skylab_team_membership_mail_queue_errors_total`.
- A send cut short by core stopping is no failure: the row keeps its attempts
  and is taken again once its lease runs out.
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
  `rejected`, `expired`, `skipped_inactive`, `skipped_no_email`,
  `skipped_not_team` (queued by a core whose rule was wider)
- `skylab_team_membership_mail_enqueued_total`,
  `skylab_team_membership_mail_queue_errors_total`,
  `skylab_team_membership_mail_precheck_errors_total`
- `skylab_team_membership_mail_coalesced_total{kind}` — `duplicate` (the same
  change already waited), `cancelled` (an opposite unsent change was
  removed); neither counts as enqueued
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
