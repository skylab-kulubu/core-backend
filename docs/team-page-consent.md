# Team page consent

A member is shown on their teams' public pages only if they asked to be
(aydınlatma s6 row 4 b, KVKK art. 5/1; ekip-yetki-devri ticket 18). The
public pages are the two rosters core answers without a sign-in, read by the
site and SkyApp:

- `GET /v1/teams/{team}/members` (a team with `public_listing=true`);
- `GET /v1/teams/{team}/leaders` (`public_listing` or `public_leaders`).

## Who is shown

| Person | Shown |
|---|---|
| holds an open `team_page` consent | in full: first and last name, profile picture, LinkedIn, university, faculty, department, leader flag |
| a Board member (in `/YK` or `/UYELER/YK`, or a Group under one; the YK roots of `AUTHZ_PRIVILEGED_ROOTS`) without the consent, **on the Board's own roster** (the team asked for is a YK root or under one) | first and last name only: the Board's names and roles are public on the club's legitimate interest (s6 row 4 a, art. 5/2-f; lawyer's answer 5; Yusuf, 2026-10-27). Their picture, LinkedIn, school and leader flag follow the consent like everyone's |
| a Board member without the consent on any other team's roster | not at all, like everyone else |
| anyone else | not at all: the roster's `count` does not count them |

Core keeps no Board duty (Başkan, Sayman…) in a field; the site's Board
cards come from the CMS.

Erased and deletion-pending people are left out as before. The admin
panel's routes (`/v1/groups/{id}/members` and the rest) do not change: a
team's leaders and the Privileged see every member.

A roster that cannot read the consents answers an error, never the
unfiltered list. The rosters answer `Cache-Control: no-store`, and the
three public team routes (`/v1/teams`, `…/members`, `…/leaders`) share one
budget of 120 requests a minute per client address.

## The consent

It is a grant of purpose `team_page` in `contact_consents`
([contact-consents.md](contact-consents.md)), so it has the evidence s6 §6
asks for (purpose, text version, source and client, times), is kept as proof
three years after it ends, is deleted with the account, and can be withdrawn
by mail to info@ (`consent-withdraw -purpose team_page`). It is not a contact
consent: its channel is `web`, only the person gives it for their own
account (source `self`, Account Center), it is active at once (no
confirmation mail), it never renews, and no mail route serves it (the
audience, renewal requests, a product's record, lookup and withdrawal by
address answer `400 consent_invalid`).

- give: `POST /v1/users/me/consents {"purpose":"team_page","textVersion":"ekip-sayfasi-v1"}`
- read: `GET /v1/users/me/consents` (the open `team_page` item)
- withdraw: `DELETE /v1/users/me/consents/team_page`: ends the account's own
  grant at once, without reading Keycloak

The rosters read it on every request: a member who consents is shown at
once, one who withdraws is hidden at once (during the transition too). While consents are off
(`CONTACT_CONSENT_KEY` unset) nobody can withdraw, so the rosters act on no
grant: only the Board's names are shown.

Text `ekip-sayfasi-v1` (the switch's label in Account Center, word for word):

> Ekip sayfalarında görüneyim: adım, soyadım, profil fotoğrafım, LinkedIn adresim, üniversitem, fakültem, bölümüm ve ekip liderliğim ekibimin sitedeki ve SKY LAB uygulamasındaki sayfasında yayımlansın.

## Transition

`TEAM_PAGE_CONSENT_GRACE_UNTIL` (RFC 3339, `2026-11-10T21:00:00Z` or
`2026-11-11T00:00:00+03:00`): until then the rosters still list in full every
member who has **not decided**, as before the rule (Yusuf, 2026-10-27: the
members listed today stay listed for 14 days while they are told, then those
who did not consent are hidden). A member who consented is shown, one who
withdrew is hidden at once. From then on the rule above holds. Unset: no
transition. Set it to the release day plus 14 days; the notice mail
(ekip-yetki-devri ticket 21) goes in the first days of it. Startup logs which
way it is.

A value that is not RFC 3339 (a date without a time, `10.11.2026`) stops
core at startup, as a wrong `AUTHZ_PRIVILEGED_ROOTS` does: the rule never
changes quietly. Check the value before it is set:

```
printf '%s\n' "$VALUE" | grep -Eq '^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?(Z|[+-][0-9]{2}:[0-9]{2})$' && echo ok
```

Known behaviour: during the transition a member added to a listed team, who
has not decided, is listed too; only the end of the transition hides them.
