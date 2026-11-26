# Finding a person's Tickets by e-mail

`POST /v1/tickets/search` with `{"email": "...", "userId": "..."}` (either or
both) answers what `GET /v1/tickets?email=…&userId=…` answers: the same rules
(the holder's own query, `ticket` read rights, drafts left out), the same
`400` when both are empty (`ticket_query_required`) or `userId` is not a
UUID (`user_id_invalid`); a body that is not JSON is `400 invalid_body`.

The e-mail goes in the body because a query string is written to access
logs (Traefik's among them) and browser history (OWASP REST Security
Cheat Sheet, "Sensitive information in HTTP requests").

`GET /v1/tickets?email=` still answers, for a client that has not moved, and
says it is deprecated (RFC 9745): `Deprecation: @1795478400` (2026-11-24)
and `Link: </v1/tickets/search>; rel="successor-version"`. `?userId=` alone
is not deprecated. The panel's `ticketsApi.list` is the only known client
and no screen calls it today.

A problem body's `instance` is the request path without its query string,
on every route: the body no longer hands back an e-mail or a share token
that came in the query.
