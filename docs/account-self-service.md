# Account Center self-service profile

`/v1/users/me` is the person's own view of their Core User shadow. It uses the
ordinary Core bearer, runs JIT identity synchronization, and like every other
self-write requires an `active` account. A `deletion_pending` or `anonymized`
subject is refused with the same generic `401`, `Cache-Control: no-store` and
`WWW-Authenticate: Bearer error="invalid_token"` everywhere: by the JIT guard
before any handler runs, and again by the handler or store if the state
changes after that check.

## Reading the profile

`GET /v1/users/me` returns the shadow fields (`id`, `email`, `firstName`,
`lastName`, `username`, `schoolEmail`, `skyNumber`, `studentCardLinked`,
`linkedin`, `university`, `faculty`, `department`, `profilePictureId`,
`profilePictureUrl`, `createdAt`, `updatedAt`) plus the person's own `phone`
and `ytuLinked` (see below).

`phone` is omitted while empty and is read-only on this route: until phone
verification exists it is written only through the privileged user card
(`PATCH /v1/users/{id}`), and `PUT`/`PATCH /v1/users/me` ignore it. It is a
field of the `/me` view alone. The privileged admin card is the only other
payload that carries a phone; `users:read` cards, public team rosters, user
search, tickets, competitors and SkyPass lookups never do. The bound
Student-card UID is likewise reported here only as `studentCardLinked`.

`PUT` and `PATCH /v1/users/me` accept `firstName`, `lastName`, `linkedin`,
`university`, `faculty` and `department` and answer with the same view.

## University, faculty and department from the YTÜ login

A person is **YTÜ-linked** once core has seen the `university` claim that
Keycloak's YTÜ Microsoft (OBS) login writes (client scope
`department_ve_university_to_jwt`). Core remembers it in `users.ytu_linked`:
Account Center's own token carries no YTÜ claims, so a request without them
says nothing about the person and never unlinks them. The view reports it as
`ytuLinked` (always present, `false` for everyone else).

- **On every request whose token carries the claims** core computes the
  current values and writes them only when they differ from the record:
  `university` as sent, `department` cleaned by `internal/ytu`, and
  `faculty` derived from the department. Program codes (`011`, `02D`, …)
  become department names, names written without spaces get them back, and
  an unknown code leaves the department empty; an unknown department leaves
  the faculty empty. People change department, so the login always wins,
  including over values the person typed before they linked YTÜ.
- **Self edits.** For a YTÜ-linked person `PUT`/`PATCH /v1/users/me` refuse
  a change to `university`, `faculty` or `department` with
  `409 Conflict`, problem `code` `ytu_managed_field`; nothing in the request
  is saved. Sending the stored value back is not a change, so a form that
  posts every field still saves `linkedin` or the name. Everyone else edits
  the three fields as before.
- **Admin edits.** `PATCH /v1/users/{id}` follows the same rule and the admin
  card carries `ytuLinked`. An admin override would be put back by the
  person's next YTÜ login, so it is refused instead; wrong data is fixed at
  the source (the person's Microsoft record, or the Keycloak attribute).
- The store enforces the rule too: `UpdateProfile` keeps the stored three
  values of a YTÜ-linked record, so a write built from an older read cannot
  revert a login that happened in between. Anonymization clears the flag
  with the values.

### One-time backfill

People who never sign in again would otherwise keep empty values in rosters.
`core-backend backfill-ytu-profile` reads every Keycloak account's
`university` and `department` attributes with core's service account
(read-only: it lists users), applies the same rule and comparison to the
existing core records, and prints counts and the department values that gave
no faculty. It never creates a record (the first request does) and skips
accounts pending deletion or anonymized. Without `-apply` it only counts.
Run it inside the running core container, which already has the environment:

```sh
docker exec <core container> ./core-backend backfill-ytu-profile          # dry run
docker exec <core container> ./core-backend backfill-ytu-profile -apply   # write
```

A second run reports every record as already in step.

## Profile picture

`POST /v1/users/me/profile-picture` (multipart field `image` or `file`)
uploads a new picture through the media service, links it to the shadow and
answers with the same view.

`DELETE /v1/users/me/profile-picture` removes it. The person's own upload is
archived first under the ordinary media lifecycle described in
[`media-lifecycle.md`](media-lifecycle.md) (hidden from current media reads
immediately, blob retained for the recovery window, purged later only when
nothing references it), then the shadow drops `profilePictureId` and
`profilePictureUrl`. Archiving before unlinking means a request that fails
half-way leaves the picture linked, so a retry sees it and finishes the job
instead of leaving a live orphan. The call is idempotent and returns
`204 No Content` even when no picture is set. There is no self-service
restore; the person uploads again.
