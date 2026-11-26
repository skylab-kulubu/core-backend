# Raffles

An Event's organizer draws winners among its attendees ("Çekiliş"): who may
win is chosen by attendance and the Ticket, the panel previews how many that
is, and the draw picks winners and alternates among them. The draw is random
(crypto/rand) and can be checked again by anyone given its record. Nothing is
mailed: the panel shows the winners, on a projector too.

## Who

Who edits the Event (`event` update: its Owner team's Leaders, its
Co-organizers, `event:manage`) or assigns Tickets (`ticket:manage`), a
person, never a service account. The door staff do neither: they take
check-ins, they do not see the attendee list a raffle is drawn from. Someone
who cannot see a draft Event is answered 404, one who sees it without the
right 403.

## Criteria

Every condition set must hold; an excluded person never wins. A cancelled or
waitlisted Ticket never takes part.

| Field | Meaning |
|---|---|
| `minCounted` | at least N check-ins on the Event's live counted Sessions: the certificate rule's count (docs/certificate-rule.md; on an Event without Sessions, 1 is a Venue entry) |
| `sessionIds`, `sessionMatch` | checked in on all (`all`) or one (`any`) of these Sessions |
| `enteredOn` | a Venue entry on that day (`YYYY-MM-DD`), on any day (`any`), or none needed (`""`) |
| `ticketType` | `REGISTERED`, `GUEST`, or both (`""`) |
| `certificateEligible` | the Event's certificate rule makes the Ticket eligible |
| `excludePastWinners` | leaves out who holds a winner's place in one of the Event's raffles (not who forfeited) |
| `excludeOrganizers` | leaves out the members of the Owner team and its sub-teams |
| `excludeDoorStaff` | leaves out the Event's door staff |
| `excludeCoOrganizers` | leaves out the members of its Co-organizer teams and its Co-organizer people |

A member Ticket is matched to an excluded person by account; a guest Ticket by
its e-mail address against theirs (primary and school), case aside. Team
members are read from Keycloak; when it does not answer, nothing is previewed
or drawn (503 `raffle_directory_unavailable`).

## Routes

All under `/v1/events/{id}/raffles`, JSON.

- `POST /preview` `{criteria}` → 200 `{eligible, excluded: {pastWinners,
  organizers, doorStaff, coOrganizers}, candidates: [{name, ticketType}],
  truncated}`. Names are masked (`Ay*** Yı***`), at most 1000; each excluded
  person is counted under the first exclusion that matched, in that order.
- `POST` `{title, criteria, winners (1–100), alternates (0–100)}` → 201 the
  raffle. Alternates are capped at what is left after the winners; fewer
  eligible than winners is 409 `raffle_too_few_candidates` (`eligible`).
- `GET` → 200 `{items}`, newest first.
- `GET /{raffleId}` → 200 the raffle: its criteria, counts, who drew it and
  when, `listHash`, `seed`, `algorithm`, and its `entries` in the draw's order
  (`position`, `status` winner/alternate/forfeited, `ticketId`, `name`,
  `maskedEmail`, `ticketType`, `promotedAt`, `forfeitedAt`, `forfeitReason`,
  `forfeitedBy`). No e-mail address is answered unmasked.
- `GET /{raffleId}/verification` → 200 `{algorithm, seed, listHash,
  ticketIds, order}`: the eligible Tickets and the drawn order, ids only.
- `POST /{raffleId}/forfeits` `{position, reason (3–300)}` → 200 the raffle.
  The winner at `position` did not come: their place is forfeited with the
  reason and the first alternate left becomes a winner. Nothing is drawn
  again. 409 `raffle_entry_not_winner`, 409 `raffle_no_alternates` (nothing
  changes).

Other codes: 400 `raffle_invalid`, 404 `raffle_not_found`, 503
`audit_unavailable`.

## The draw (`sky-raffle-v1`)

1. The eligible Tickets' ids, lowercase, sorted ascending, joined by `\n`
   (no newline at the end); `listHash` is their SHA-256.
2. `seed` is 32 bytes from crypto/rand, drawn under the Event's raffle lock
   (two draws at once never pass over the same past winners).
3. Numbers: block *n* = SHA-256(seed ‖ listHash ‖ *n* as 8 big-endian bytes),
   *n* from 0; each number is a block's first 8 bytes, big-endian. A number
   in [0, m) takes the next number until it is below 2⁶⁴ − (2⁶⁴ mod m), then
   its remainder by m (no modulo bias).
4. A Fisher–Yates shuffle stopped after winners + alternates places: place
   *i* swaps with *i* + number(n − *i*). The first places win, the rest are
   alternates in order.

The record keeps the eligible ids, the hash, the seed and the algorithm, so
the panel's "Tarayıcıda doğrula" draws the order again in the browser and
compares it; a test vector shared by core and the panel pins both. The seed
is not shown before the draw: the draw cannot be repeated quietly, as every
draw is kept and listed and none is deleted.

## Record

`event_raffles` keeps each draw (criteria, eligible ids, hash, seed,
algorithm, who and when); `event_raffle_entries` its places. Ids only, no
name nor address. Each draw and each forfeit opens a Team audit row first
(`raffle.draw`, `raffle.forfeit`; docs/team-audit.md) on a Ticket operator's
basis, with the Event, the raffle's id and counts and, for a forfeit, the
two positions, never a name nor the reason; without the row nothing is
written (503 `audit_unavailable`).

Account erasure forgets who drew or forfeited, and a forfeit's reason on the
erased person's Ticket; the Ticket stays anonymous, so the raffle stays
verifiable. The raffles go with their Event.
