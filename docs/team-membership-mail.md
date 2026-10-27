# Membership mail (team, role and group)

A person added to a Group, or removed from one, through core's membership
routes gets a mail (Yusuf, 2026-10-12: every Group, no exceptions). The
Group's kind picks the SkyMail template:

| Kind | Groups | Template | Subject |
| --- | --- | --- | --- |
| `team` | a team and its `LIDERLER`/`KOORDINATORLER` (table below) | `club.team-membership` | SKY LAB takım üyeliğinde değişiklik |
| `role` | a Privileged root and every Group under one (`AUTHZ_PRIVILEGED_ROOTS`; unset, as in production, the six defaults `/ADMIN`, `/YK`, `/DK`, `/UYELER/ADMIN`, `/UYELER/YK`, `/UYELER/DK`, of which production's Keycloak has `/ADMIN`, `/UYELER/YK` and `/UYELER/DK`) | `club.role-membership` | SKY LAB yönetim yetkilerinde değişiklik |
| `group` | every other Group: the club (`/UYELER`), an area (`/UYELER/ARGE`), an area's own `LIDERLER`, technical Groups (`ESKI-EDITORLER`), Groups outside `/UYELER` | `club.group-membership` | SKY LAB grup üyeliğinde değişiklik |

`teammail.KindOf` decides, from the path alone: a team first (`TeamOf`), then
a Privileged root or a Group under one, then anything else. With the default
roots a `/UYELER/ADMIN` created later gets the role mail (`Role` = `admin`);
only an `AUTHZ_PRIVILEGED_ROOTS` that leaves it out makes it a plain Group,
with the group mail, as authz then counts it no Privileged Group either. Until 2026-10-12
only teams were mailed; a person added to `/UYELER/DK` got nothing, which is
what this change ends.

The routes:

- `POST /v1/groups/:groupId/members` (admin panel: add a person to a Group)
- `DELETE /v1/groups/:groupId/members/:userId` (remove a person; through a
  parent roster this removes the subgroup they sit in, see CONTEXT.md
  "Source group")

A change made in Keycloak's own console sends nothing: core does not see it.

A Privileged Group's membership change also tells ADMIN and that root's
other members, by the privilege notice (`club.privilege-change`,
[privilege-change-mail.md](privilege-change-mail.md)); the person changed
gets only the role mail above. While that notice is on, a Privileged Group
is read for the notice even with the role mail off.

## Which Groups are teams

`teammail.TeamOf` decides, from the Group's path alone (`no` rows get the
role or the group mail, by the table above):

| Path | Team? | `TeamName` |
| --- | --- | --- |
| `/UYELER/<area>/<team>` (e.g. `/UYELER/ARGE/WEBLAB`) | yes | code · Turkish name: `WEBLAB · Web Geliştirme` |
| `/UYELER/<area>/<team>/LIDERLER` | yes | the team's: `WEBLAB · Web Geliştirme` (`Role` = `leader`) |
| `/UYELER/<area>/<team>/KOORDINATORLER` | yes | the team's: `WEBLAB · Web Geliştirme` (`Role` = `coordinator`) |
| a sub-team below a team (e.g. `/UYELER/ARGE/ALGOLAB/AGC`, and its `LIDERLER`) | yes | the sub-team's: `AGC` |
| `/UYELER` (the club), `/UYELER/<area>` (ARGE, ORGANIZASYON, …, and technical Groups such as `ESKI-EDITORLER`), an area's own `LIDERLER` | no: group mail | |
| a Privileged root and everything under it (`AUTHZ_PRIVILEGED_ROOTS`, by default `/ADMIN`, `/YK`, `/DK`, `/UYELER/ADMIN`, `/UYELER/YK`, `/UYELER/DK`; e.g. `/UYELER/YK/BASKAN`). A Group named `ADMIN`, `YK` or `DK` elsewhere is a team like any other ([authz-roles.md](authz-roles.md#privileged-roots-authz_privileged_roots)) | no: role mail | |
| anything outside `/UYELER` | no: group mail | |

The code is the team Group's name and the Turkish name its `display_name_tr`
(the name the public team list shows); without a Turkish name, or with one
equal to the code, the code alone (Yusuf, 2026-10-09). A leader subgroup is
named by its team; the role travels in `Role`. The team is read from the
directory when the mail is sent, so a team renamed in between is mailed under
its new name; one that is gone is mailed under the last segment of its path.
A row keeps the Group's Keycloak id too (`group_id`, migration
`20261027190000`; ADR 0065 ticket 07): the Group is found by it first, so a
Team moved or given a new code within the 72 hours is mailed by where it is
now (its kind, team and role read from the new path). A row without one (an
older core's) or whose id is gone is read by its path as before.

## Variables

### Team mail (`club.team-membership`)

| Variable | Value |
| --- | --- |
| `TeamName` | as in the table above |
| `Action` | `added` or `removed` (the template renders "eklendi" / "çıkarıldı") |
| `Role` | `member` (the team itself), `leader` (`LIDERLER`) or `coordinator` (`KOORDINATORLER`): the template words its text by it, since only leaders and coordinators get management rights |
| `EffectiveAt` | the day of the change in Europe/Istanbul, `dd.MM.yyyy` |
| `LeaderName` | the full name of the person who made the change ("İşlemi yapan"); empty, and the line left out, for a service account or someone core may no longer name |

### Role mail (`club.role-membership`)

| Variable | Value |
| --- | --- |
| `GroupName` | the changed Group itself, named like a team: code · `display_name_tr` (`YK · Yönetim Kurulu`), the code alone without a Turkish name; a subgroup by its own name (`BASKAN`) |
| `Role` | the root the Group is or sits under, by its last segment: `board` (YK, Yönetim Kurulu), `audit` (DK, Denetim Kurulu), `admin` (ADMIN), `privileged` (a root the template has no words for) |
| `Action` | `added` or `removed` |
| `EffectiveAt` | as in the team mail |
| `ActorName` | who made the change, as `LeaderName` in the team mail ("İşlemi yapan"); empty for a service account or someone core may no longer name |

The text says what the Group is ("Yönetim Kurulu'na eklendin"), that its
management rights were added to or taken from the account, and asks the
person to tell an administrator if they did not expect the change.

### Group mail (`club.group-membership`)

| Variable | Value |
| --- | --- |
| `GroupName` | as in the role mail (`ARGE · Ar-Ge`, `ESKI-EDITORLER`) |
| `Action` | `added` or `removed` |
| `EffectiveAt` | as in the team mail |
| `ActorName` | as in the role mail |

The Group is read from the directory when the mail is sent; one that is gone
by then is named by the last segment of its path.

The mail goes to the person's primary e-mail (Keycloak's `email`), with core's
profile name as the recipient name.

## One mail per change

- **Add:** before the write, core reads the Group (skipped when the route
  names it by a path whose kind is off) and the person's direct Groups,
  together bounded to 3 s. Already a direct member: Keycloak takes the
  add again, nothing changed, no mail. A read that fails or times out lets the
  add go on without a mail, and is logged without the person and counted
  (`skylab_team_membership_mail_precheck_errors_total`).
  Joining a team's `LIDERLER` while a member of the team is a change of its
  own and is mailed.
- **Remove:** only when the person is on the roster; the mail names the Group
  they were removed from (the subgroup they sat in).
- A write that fails sends nothing. A Group whose kind is off costs no
  membership read (and no read at all when named by path).
- One row waits per person, Group and action: the same change made again
  before its mail went (a double click on add) is not queued twice
  (`coalesced_total{kind="duplicate"}`).
- Opposite changes cancel out while unsent: a removal made while the add's
  mail still waits, never taken by a pass and never failed, deletes that
  row and is not queued (`coalesced_total{kind="cancelled"}`). So add,
  remove, add again before any mail went sends one "added" mail.
- The later change wins. A change that may already have reached the person
  (being sent now, failed once after SkyMail may have taken it, or left by
  a core that stopped mid-send) is not cancelled; the opposite one is
  queued after it. That one is not taken while the first is being sent (at
  most its two-minute lease), and once the first is no longer being sent
  it is dropped unsent and the later one goes. So the last mail always
  tells the current state: a person removed while their "added" mail
  failed gets "removed" alone, never "added" after it, and one whose
  "added" went but was retried still gets "removed".
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

- The queue is `team_membership_mails`: ids (the Group's too), the Group's path, the action,
  the time and the kind it was queued for (`kind`: `team`, `role`, `group`;
  migration `20261012170000`, default `team`), never an address or a name.
  The sending core's rule (`KindOf`) picks the mail; the column says what
  waits. A pass takes one row at a time
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
- At send time a row whose kind has been switched off since it was queued
  is skipped (`skipped_kind_off`). The person is skipped — the row deleted,
  nothing sent — when they are erased, being erased (`deletion_pending`), hard-purged, disabled in
  Keycloak or gone (`skipped_inactive`), or have no primary e-mail
  (`skipped_no_email`). A Keycloak service account has no primary e-mail and
  lands here.
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
| `SKYMAIL_TEAM_MEMBERSHIP_TEMPLATE_KEY` | `club.team-membership` | team mail off |
| `SKYMAIL_ROLE_MEMBERSHIP_TEMPLATE_KEY` | `club.role-membership` | role mail off |
| `SKYMAIL_GROUP_MEMBERSHIP_TEMPLATE_KEY` | `club.group-membership` | group mail off |

It needs core's SkyMail client (`KEYCLOAK_URL`, `KEYCLOAK_REALM`,
`KEYCLOAK_CLIENT_ID`, `KEYCLOAK_CLIENT_SECRET`, `SKYMAIL_URL`). Startup says
which state it is in: `team membership mail: on (template club.team-membership;
role club.role-membership; group club.group-membership)` (a kind set empty
reads `off`), or `team membership mail: off (…)` when SkyMail is not
configured or all three keys are empty. Off, the membership routes read
nothing for it. A kind that is off is neither read for nor queued. There is
no template id fallback.

Before 2026-10-12 `SKYMAIL_TEAM_MEMBERSHIP_TEMPLATE_KEY=` (empty) turned the
whole mail off; now it turns the team mail off only. To turn everything off,
set all three empty.

## Metrics (`/v1/metrics`, internal)

The names keep the `team_membership` prefix they had when only teams were
mailed, so existing readers go on working; they count every kind.

- `skylab_team_membership_mail_enabled` — 1 on, 0 off
- `skylab_team_membership_mail_kind_enabled{mail_kind}` — `team`, `role`,
  `group`: 1 when the kind has a template key
- `skylab_team_membership_mail_total{outcome}` — `sent`, `failed` (retried),
  `rejected`, `expired`, `skipped_inactive`, `skipped_no_email`,
  `skipped_kind_off` (the kind was switched off after the row was queued),
  `skipped_not_team` (kept for its readers, always 0 since every Group is
  mailed)
- `skylab_team_membership_mail_by_kind_total{mail_kind,outcome}` — the same
  outcomes of a send by kind (`expired` has no kind: rows are dropped by
  age)
- `skylab_team_membership_mail_enqueued_total`,
  `skylab_team_membership_mail_enqueued_by_kind_total{mail_kind}`,
  `skylab_team_membership_mail_queue_errors_total`,
  `skylab_team_membership_mail_precheck_errors_total`
- `skylab_team_membership_mail_coalesced_total{kind}` — `duplicate` (the same
  change already waited), `cancelled` (an opposite unsent change was
  removed); neither counts as enqueued. Its `kind` label is older and means
  how a change was coalesced; the mail's kind is the `mail_kind` label of the
  series above.
- `skylab_team_membership_mail_backlog`,
  `skylab_team_membership_mail_oldest_age_seconds`,
  `skylab_team_membership_mail_last_success_timestamp_seconds`

A growing `oldest_age_seconds` with `failed` rising means SkyMail is refusing:
the `skymail_call_failed` line with `"kind":"team_membership"` (or
`role_membership`, `group_membership`) carries the status ([skymail-templates.md](skymail-templates.md)).

## Sending a mail for an earlier change

Nothing is sent for changes made before this mail existed. To send one by
hand, remove the person from the team and add them again in the admin panel:
that is two real changes and sends two mails (removed, then added). To send
only the "added" mail, queue the row directly (production database, `core`):

```sql
INSERT INTO team_membership_mails (id, subject_id, actor_id, group_path, action, occurred_at, next_attempt_at, kind)
VALUES (gen_random_uuid(), '<person id>', '<actor id or NULL>', '/UYELER/<area>/<team>', 'added', '<when>', now(), 'team');
```

`kind` is `team`, `role` or `group` by the Group (it only describes the row;
the worker picks the mail by the path).

`occurred_at` is the day the mail shows; keep it within 72 hours of now or the
worker drops the row as expired before sending it.

## Rollout

1. SkyMail first: merge the two templates (`club.role-membership`,
   `club.group-membership`, skymail-frontend) and run the production template
   seed.
2. Then core. A core whose template is not seeded yet gets `404` from
   SkyMail: the row waits and is retried (30 s doubling to 30 min) for 72
   hours, so a seed that lands within that time sends every waiting mail and
   nothing is lost. Core does not skip a kind for a missing template, because
   it cannot tell "not seeded yet" from a short SkyMail outage; to hold a
   kind back on purpose, set its key empty.
3. While the old and the new core both run (seconds, during the rollout), an
   old core may take a role or group row and drop it as `skipped_not_team`;
   a change made in those seconds may get no mail.

## Rollback

An older core image (before 2026-10-12) mails teams only. Its worker takes
every row and drops a role or group row as `skipped_not_team`, deleting it
without a mail. Rolling core back therefore loses the role and group mails
still waiting, for example ones retried because SkyMail answered `404` or
was down. Before rolling back, wait until
`skylab_team_membership_mail_backlog` is 0 (or accept that loss). To stop
new role and group mails without a rollback, set
`SKYMAIL_ROLE_MEMBERSHIP_TEMPLATE_KEY=` and
`SKYMAIL_GROUP_MEMBERSHIP_TEMPLATE_KEY=` (empty) instead. The migration
needs no rollback: the older core writes rows without `kind` and they take
the default `team`.

## Compared with other products

What matches and where we differ (Yusuf's decision, 2026-10-12):

- Microsoft 365 Groups send a welcome mail when a member is added (it can be
  turned off per group); Google Groups can send an optional welcome message;
  GitHub tells a person of their team and organization membership
  (the organization invitation mail, team notifications). Our team and
  group mails follow these.
- Microsoft Entra ID sends no mail when someone is added to a security group,
  nor, by default, when an administrator role is assigned (Privileged
  Identity Management alerts are opt-in). **Deviation:** we mail every Group,
  the Privileged ones included (`club.role-membership`), with no per-Group
  switch: membership in YK, DK or ADMIN gives management rights, so the
  person hears of it, and an unexpected change is noticed by the one it
  concerns.
- Microsoft 365 and Google Groups send no mail on a removal by default.
  **Deviation:** we mail removals too, as the team mail always has.
