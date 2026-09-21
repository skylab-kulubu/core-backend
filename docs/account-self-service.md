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
`profilePictureUrl`, `createdAt`, `updatedAt`) plus the person's own `phone`.

`phone` is omitted while empty and is read-only on this route: until phone
verification exists it is written only through the privileged user card
(`PATCH /v1/users/{id}`), and `PUT`/`PATCH /v1/users/me` ignore it. It is a
field of the `/me` view alone. The privileged admin card is the only other
payload that carries a phone; `users:read` cards, public team rosters, user
search, tickets, competitors and SkyPass lookups never do. The bound
Student-card UID is likewise reported here only as `studentCardLinked`.

`PUT` and `PATCH /v1/users/me` accept `firstName`, `lastName`, `linkedin`,
`university`, `faculty` and `department` and answer with the same view.

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
