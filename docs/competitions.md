# Competitions

**A Competition belongs to one Event; its score is computed from an
append-only ledger** (ADR-0068, e-skylab). The Event gives it its Owner team,
Season and Tickets; the Competition stores none of them again. This first
slice is what AGC's weekly event needs: one Stage scored by hand
(`manual`), individual Entrants from the Event's Tickets, guests included, and
the organizers' live table. Result publication and the public routes (ticket
04), bulk scoring from attendance (03), Series (05) and the `/v1/competitors/me`
bridge for sky-app (06) come next. The old `/v1/competitors…` routes are
unchanged.

## Who

Every route needs a signed-in organizer of the Competition: a Leader
(`LIDERLER`, `KOORDINATORLER`) of the Event's Owner team, or a holder of
`competition:manage` ([authz-roles.md](authz-roles.md)). Membership of the
team alone manages nothing. A Competition of an Event without an Owner team
is managed by the Privileged only (ADR-0018). The admin panel reads
`competition.manage` from `GET /v1/users/me/capabilities` (team scopes) to
show the Competition tab; it never decides from the token. Anonymous: `401`;
anyone else: `403`.

## Model

| Thing | What |
|---|---|
| Competition | `name` (1–200), `description` (≤ 5000), `entrantMode` (`individual` only for now), `status`, `stages`, `ownerTeam` (the Event's, read-only) |
| Stage | one per Competition for now (`position` 1, `strategy` `manual`, `config` `{}`); the panel does not show it |
| Entrant | one Ticket of the Event (`REGISTERED` or `GUEST`); `displayName` is the person's name now, `Silinmiş kullanıcı` once their account is being deleted or a guest's name was erased |
| Entry | one ledger row: `source` `manual`, `points`, who and when; a correction revokes the row with its reason and writes a new one that names it (`replacesId`). Nothing is edited or deleted |

**Points** are numbers with at most three decimals (`30`, `50.5`, `0.125`),
within ±1 000 000 000. Core keeps thousandths as integers, so equal scores are
equal. A string, `null`, an exponent or a fourth decimal is `400`
(`code: invalid_points`).

**Ranking:** highest score first; equal scores share a rank (1, 1, 3). An
active Entrant without a score is listed after the ranked ones with `rank` and
`score` `null`.

## Status

```
draft ──open──▶ open ──start──▶ running ──close──▶ closed ──reopen──▶ running
  │ start / close skip ahead                         │
  └──────────────── cancel (reason) ◀─────────────────┘   cancelled is final
archive / restore: hidden from reads, status kept
```

| Status | Entrants join | Scores are written |
|---|---|---|
| `draft`, `open`, `running` | yes | yes |
| `closed` | no (`reopen` first) | yes: results are mostly typed in after the Event |
| `cancelled` | no | no |

Adding Entrants no longer depends on the Event being active. A wrong status is
`409` with `code: competition_status`. An archived Event hides its
Competitions (`404`).

## Routes

| Route | Body | Answer |
|---|---|---|
| `GET /v1/events/{eventId}/competitions[?lifecycle=current\|inactive\|all]` | | `200` list (oldest first) |
| `POST /v1/events/{eventId}/competitions` | `{name, description?, entrantMode?}` | `201` Competition; `404` without the Event |
| `GET /v1/competitions/{id}` | | `200` |
| `PATCH /v1/competitions/{id}` | `{name?, description?}` | `200` |
| `POST /v1/competitions/{id}/open`, `/start`, `/close`, `/reopen` | | `200` |
| `POST /v1/competitions/{id}/cancel` | `{reason}` | `200`; no reason: `400 reason_required` |
| `POST /v1/competitions/{id}/archive`, `/restore` | | `204`, `200` |
| `GET /v1/competitions/{id}/entrants` | | `200` |
| `POST /v1/competitions/{id}/entrants` | `{ticketIds: [..]}` (1–500) | `201` the new Entrants; all or none: a Ticket already in the Competition is `409 ticket_taken`, a Ticket of another Event `400` |
| `POST /v1/competitions/{id}/scores` | `{entrantId, points, reason?, stageId?}` | `201` the new Entry. Replacing a live score needs `reason` (`400 reason_required`); the old row is revoked with it |
| `POST /v1/competitions/{id}/entries/{entryId}/revoke` | `{reason}` | `200`; already revoked: `409` |
| `GET /v1/competitions/{id}/entries` | | `200` every row, revoked ones included, oldest first: why a score changed |
| `GET /v1/competitions/{id}/leaderboard` | | `200` `{competitionId, stageId, strategy, rows: [{rank, entrantId, displayName, members, score}]}` |

Errors are RFC 9457 problem details; `400` and `409` carry a `code` where the
panel can act on it.

## Storage

Migration `20261012140000_competitions` adds `competitions`,
`competition_stages`, `competition_entrants`, `competition_entrant_members`,
`competition_submissions` and `competition_adjustments` (no route writes
adjustments yet). The database keeps the rules on its own:

- a Ticket is in one Entrant of a Competition, and is a Ticket of its Event;
- one live manual score per Entrant and Stage;
- ledger rows are never deleted or edited; a row is revoked once, with a
  reason; purging an account only clears the actor columns;
- actor columns follow the account lifecycle guard (no new link to an account
  being deleted).

The tables are new and nothing else reads them, so the release is backward
compatible. The down migration refuses while a Competition exists.
