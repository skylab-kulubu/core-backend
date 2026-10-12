# Privilege notice (role mappings and Privileged Group membership)

When management rights are given or taken through core, the people who
watch over them hear of it by mail (grup-yonetimi 05; Yusuf, 2026-10-12:
"UYELER'e mail gitmemeli; sektördeki gibi"). Two rules:

| Change | Route | Told | Not told |
| --- | --- | --- | --- |
| **1.** A Group's role mappings changed: a client role mapped or unmapped, on any Group (`/UYELER` included) | `PUT /v1/groups/:groupId/client-roles` | the members of ADMIN | the Group's own members, however many (`/UYELER` has hundreds) |
| **2.** A person added to or removed from a Privileged Group: a root of `AUTHZ_PRIVILEGED_ROOTS` (ADMIN, YK, DK) or a Group under one | `POST /v1/groups/:groupId/members`, `DELETE /v1/groups/:groupId/members/:userId` (and the delegated routes, which never reach a Privileged Group today) | the members of ADMIN and every member of that root's tree (for `/UYELER/YK/BASKAN`, all of `/UYELER/YK`) | the person changed: they get their own role membership mail, `club.role-membership` ([team-membership-mail.md](team-membership-mail.md)) |

In both, "the members of ADMIN" are everyone in the tree of each Privileged
root named `ADMIN` (by default `/ADMIN` and `/UYELER/ADMIN`; a root that does
not exist has nobody). Whoever made the change is left out, and so is the
person added or removed. Each recipient gets one mail per change: a person
in ADMIN and in YK gets one, and one role mapping write that maps three roles
and unmaps one is one mail listing all four. A recipient without a primary
e-mail (a Keycloak service account), or who is erased, being erased,
disabled or gone, is skipped, as in the membership mail.

A write that changed nothing sends nothing: the same mappings again, or an
add of someone already in the Group (core reads the Group's mappings, and
the person's Groups, before the write, bounded to 3 s; a read that fails lets
the write go on without a notice and counts
`skylab_privilege_notice_mail_precheck_errors_total`). A role mapping write
that fails is read again: Keycloak writes client by client, so whatever did
change is told. A change made in Keycloak's own console sends nothing until
grup-yonetimi 10 brings those changes in through the same function.

Unlike the membership mail, opposite changes do not cancel out: an add and a
removal before either notice went are both told, since a right held for a
moment was held.

## The mail (`club.privilege-change`)

| Variable | Value |
| --- | --- |
| `Change` | `member-added`, `member-removed` or `roles-changed` |
| `GroupName` | the changed Group, named as in the membership mails: code · `display_name_tr` (`YK · Yönetim Kurulu`), a subgroup by its own name (`BASKAN`) |
| `GroupId` | the Group's id, for the panel link (`https://admin.yildizskylab.com/groups/<id>`); empty for a Group gone since |
| `Root` | member events: the root the Group is or sits under, as the role mail's `Role` (`board`, `audit`, `admin`, `privileged`); empty for a role mapping |
| `SubjectName` | member events: the full name of the person added or removed; empty for someone core may no longer name |
| `ActorName` | who made the change; empty for a service account or someone core may no longer name |
| `OccurredAt` | when, in Europe/Istanbul, `dd.MM.yyyy HH:mm` |
| `RolesAdded`, `RolesRemoved` | role mapping: the roles as `client:role` (a realm role as `realm:ROLE`), sorted, joined by `, `; empty otherwise |
| `AdminRole` | role mapping: `yes` when a changed role is a known admin role by authz's role classes (ADR 0065 decision 10, `authz.ClassifyRole` = `admin`: core's permission roles, `audit:read`, a Site's `cms:access` and `content:write`, any `client:admin`, any `realm-management` role, Forms', SkyMail's and Place's admin roles; see `authz.AdminRoles`), or a realm role (`authz.ClassifyRealmRole`, always admin); `no` for an app role and for a client role nobody has classified (delegation treats that one as protected, but the mail says `yes` only when it is sure) |
| `MemberCount` | role mapping: how many people sit in the Group's tree when the notice was read for its recipients (they are counted, never told); empty otherwise, and empty when the tree could not be read within 20 s (the count is best effort: the notice goes without it) |

Every key is always sent. No e-mail address is in the mail.

## Delivery

```
role mapping / Privileged membership write ──▶ Keycloak ──▶ INSERT privilege_notice_mails (one row, no recipient)
                                                                │
                               worker (the membership mail's, same wake-up and poll)
                                                                │
          read ADMIN's tree (+ the root's tree, or the Group's count) ──▶ one row per recipient ──▶ POST SkyMail
```

- The queue is `privilege_notice_mails` (migration `20261012190000`): ids,
  the Group's path and role names, never an address or a name. A change
  waits as one row; the worker reads its recipients and, in one statement
  fenced by its claim, replaces it with one row per recipient. Those are
  sent like the membership mail's rows: one at a time, leased for two
  minutes, retried after 30 s doubling to 30 min, dropped after 72 hours.
- A table of its own on purpose: an older core takes every row of
  `team_membership_mails` and would send a notice row's recipient a
  "you were added" mail. An older core never reads this table.
- A roster of the recipients (ADMIN's, a root's) that cannot be read leaves
  the change waiting for a retry. The count of a role mapping's Group is
  best effort: a tree that cannot be read within 20 s (a large `/UYELER`, a
  slow Keycloak) leaves `MemberCount` empty and the notice goes at once
  (`skylab_privilege_notice_mail_count_errors_total`).
- SkyMail's `404` (the template not seeded yet) retries like any failure, so
  a seed within 72 hours sends every waiting notice; `400`/`422` drops the
  row as `rejected`.
- The notice switched off while rows wait: each is dropped at its turn
  (`skipped_kind_off`).
- Account erasure (`anonymize_core`) deletes the rows that go to the person
  and forgets them as the subject and the actor.
- No audit row is written for a notice: the change's own row
  ([team-audit.md](team-audit.md)) is the record; the queue's metrics show
  the mail.

## Configuration

| Variable | Unset | Empty |
| --- | --- | --- |
| `SKYMAIL_PRIVILEGE_NOTICE_TEMPLATE_KEY` | `club.privilege-change` | notice off |

It needs core's SkyMail client, like the membership mail. The startup line
says `team membership mail: on (…; privilege notice club.privilege-change)`
or `privilege notice off`. With the notice off, a role mapping write reads
nothing for it, and a Privileged Group's membership is read only for the
role mail.

## Metrics (`/v1/metrics`, internal)

- `skylab_privilege_notice_mail_enabled` — 1 on, 0 off
- `skylab_privilege_notice_mail_total{outcome}` — `read` (a change read for
  its recipients), `no_recipients`, `sent`, `failed` (retried), `rejected`,
  `expired`, `skipped_inactive`, `skipped_no_email`, `skipped_kind_off`
- `skylab_privilege_notice_mail_enqueued_total{event}` — `member_added`,
  `member_removed`, `roles_changed`
- `skylab_privilege_notice_mail_recipients_total` — rows made by reading
  changes for their recipients
- `skylab_privilege_notice_mail_queue_errors_total`,
  `skylab_privilege_notice_mail_precheck_errors_total`,
  `skylab_privilege_notice_mail_count_errors_total` (a Group's tree that
  could not be counted; the notice went without the count)
- `skylab_privilege_notice_mail_backlog`,
  `skylab_privilege_notice_mail_oldest_age_seconds`

A SkyMail refusal is logged as `skymail_call_failed` with
`"kind":"privilege_notice"`, without the recipient.

## Rollout

1. SkyMail first: merge the template (`club.privilege-change`,
   skymail-frontend) and run the production template seed.
2. Then core. The migration creates the table. Notices queued before the
   seed wait and are retried for 72 hours.

## Rollback

An older core does not read `privilege_notice_mails`: notices waiting there
stay until a newer core sends them or the 72 hours pass. To stop the notice
without a rollback, set `SKYMAIL_PRIVILEGE_NOTICE_TEMPLATE_KEY=` (empty).
The down migration drops the table and whatever waits in it.

## Compared with other products

What matches, and every deviation (benchmarked as the club decided,
2026-10-12):

- **Microsoft Entra PIM** notifies administrators when a privileged role is
  assigned, and the person assigned; PIM for Groups does the same for a
  group's membership. **Matches:** rule 1 tells ADMIN alone, never the
  Group's members; rule 2 tells ADMIN, and the person changed gets their
  own mail.
- **Google Workspace** Alert Center has a "User granted Admin privilege"
  rule, off by default. **Matches** in kind (a mail to the administrators).
- **Deviation 1 (peer visibility, GY5):** on a Privileged Group's
  membership, the root's other members are told too. PIM does not tell a
  group's other members. In a club, the board seeing who joins the board is
  the point: a single YK member can add someone today without anyone
  hearing of it.
- **Deviation 2:** on by default (Google's rule is off by default), with one
  key to turn it off.
- **Deviation 3:** a role mapping is told for every Group, ordinary ones
  too, not only for administrator roles; `AdminRole` says whether a known
  admin role (Entra's directory role counterpart) changed.
- **Deviation 4:** the recipients are read when the notice is sent (seconds
  after the change), not frozen at the change.
- **Deviation 5:** opposite changes do not cancel out (the membership mail's
  do).
