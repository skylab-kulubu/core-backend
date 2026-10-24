# SkyPass card

A person may bind one card (YTÜ Student card or any card whose chip UID can be
read) so the door finds them by it. Binding is optional; core stores only the
chip UID (`users.student_card_uid`) and never returns it: the person's own view
and every other payload say only `studentCardLinked`.

- `POST /v1/skypass/card-bind` `{"uid": "04AABBCCDD"}` binds or replaces the
  caller's card (`200` with the user; `409` when another person holds the
  UID). With `userId` and an empty `uid`, `users:manage` (or the Privileged
  Group) clears someone else's card.
- `DELETE /v1/skypass/card-bind` removes the caller's own card: the UID is set
  to NULL, the door no longer finds the person by it, and anyone may bind that
  card again. No body; `204`, also when no card was bound. `401` without a
  token; a `deletion_pending` or erased account gets the same generic `401` as
  every self-write ([account-self-service.md](account-self-service.md)).
  Nothing about the card is logged or written to an audit record.

Account erasure clears the UID too ([account-lifecycle.md](account-lifecycle.md)),
and a `deletion_pending` person's card no longer matches at the door.
