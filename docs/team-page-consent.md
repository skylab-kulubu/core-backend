# Team page visibility

Every member is shown on their teams' public pages unless they object
(aydınlatma s6 row 4 b: the club's legitimate interest, KVKK art. 5/2-f,
with the person's right to object; Yusuf, 2026-10-31, lawyer approved;
ekip-yetki-devri ticket 18). Nobody disappears on a date: the transition
of the consent rule (2026-10-27 to 2026-10-31) is gone. The public pages
are the two rosters core answers without a sign-in, read by SkyApp (the
site reads neither: its Board cards are the CMS's):

- `GET /v1/teams/{team}/members` (a team with `public_listing=true`);
- `GET /v1/teams/{team}/leaders` (`public_listing` or `public_leaders`).

## Who is shown

| Person | Shown |
|---|---|
| a member who does not object | in full: first and last name, profile picture, LinkedIn, university, faculty, department, leader flag |
| a Board member (in `/YK` or `/UYELER/YK`, or a Group under one; the YK roots of `AUTHZ_PRIVILEGED_ROOTS`) who objects, **on the Board's own roster** (the team asked for is a YK root or under one) | first and last name only: the Board's names and roles are public on the club's legitimate interest (s6 row 4 a, art. 5/2-f; lawyer's answer 5; Yusuf, 2026-10-27). Their picture, LinkedIn, school and leader flag are hidden like everyone's who objects |
| a Board member who objects, on any other team's roster | not at all, like everyone else |
| anyone else who objects | not at all: the roster's `count` does not count them |

Core keeps no Board duty (Başkan, Sayman…) in a field; the site's Board
cards come from the CMS.

Erased and deletion-pending people are left out as before. The admin
panel's routes (`/v1/groups/{id}/members` and the rest) do not change: a
team's leaders and the Privileged see every member.

A roster that cannot read the objections answers an error, never the
unfiltered list. The rosters answer `Cache-Control: no-store`, and the
three public team routes (`/v1/teams`, `…/members`, `…/leaders`) share one
budget of 120 requests a minute per client address.

## The objection

It is a grant of purpose `team_page_objection` in `contact_consents`
([contact-consents.md](contact-consents.md)). It is not a consent: it is
kept there for the same evidence (purpose, text version, source and
client, times), is kept as proof three years after it ends, and is deleted
with the account. Its channel is `web`, only the person records it for
their own account (source `self`, Account Center), it is active at once
(no confirmation mail), it never renews, and no mail route serves it (the
audience, renewal requests, a product's record, lookup and withdrawal by
address answer `400 consent_invalid`).

Account Center's "Ekip sayfaları" switch is on unless the person has an
open objection:

- off (object): `POST /v1/users/me/consents {"purpose":"team_page_objection","textVersion":"ekip-sayfasi-itiraz-v1"}`
- read: `GET /v1/users/me/consents` (an open `team_page_objection` item: off)
- on again (lift it): `DELETE /v1/users/me/consents/team_page_objection`:
  ends the account's own objection at once, without reading Keycloak

The rosters read the objections on every request: who objects is hidden
at once, who lifts it is shown at once. They read them with consents off
(`CONTACT_CONSENT_KEY` unset) too: nobody can lift an objection then, so
who objected stays hidden. No command records an objection for a person
who asks by mail: they turn the switch off themselves. Nor does staff lift
one: `consent-withdraw -purpose team_page_objection` is refused (exit 2),
since ending it shows the person again.

Text `ekip-sayfasi-itiraz-v1` (the switch in Account Center, its label and
description word for word; what the person turned off):

> Ekip sayfalarında görün. Adın, soyadın, profil fotoğrafın, LinkedIn adresin, üniversiten, fakülten, bölümün ve ekip liderliğin ekibinin SKY LAB uygulamasındaki sayfasında yer alır.

## The consent before it (2026-10-27 to 2026-10-31)

Until 2026-10-31 a member was shown only with a `team_page` consent (text
`ekip-sayfasi-v1`), with a 14-day transition for who had not decided. That
purpose is no longer taken: giving or withdrawing it answers
`400 purpose_not_enabled`. Its rows stay as proof and keep their meaning:

- an open `team_page` consent: shown, as everyone who does not object;
- a member whose `team_page` grants had all ended (they withdrew): carried
  over by migration `20261031200000` as an open objection, from the time
  of the last withdrawal, on the text they withdrew (`ekip-sayfasi-v1`).
  The `team_page` rows are not changed. Turning the switch on lifts it.

`TEAM_PAGE_CONSENT_GRACE_UNTIL` is no longer read by the rosters or at
startup: it may stay set (production has it) or go, and a bad value no
longer stops core. Nothing reads it any more.

## The notice (not to be used)

**Refused since 2026-10-31:** nobody is hidden on a date any more, so
there is nothing to tell (Yusuf: the team page notice is not sent).
`./core-backend team-page-notice`, dry or `-apply`, refuses every run with
exit 2 ("ekip sayfaları varsayılan açık; bu bildirim artık gönderilmez")
whatever the environment holds, and reads or writes nothing: no Keycloak,
database or SkyMail. The ledger `team_page_notices` and its account
erasure stay. What follows is how it worked.


Every member a public roster lists who has **not decided** is told once,
on the first day of the transition, that the pages will show only those who
consent (ekip-yetki-devri ticket 21). Who consented stays listed and who
withdrew is hidden already, so neither is told. It is a transactional mail:
it tells a person of a change in how their own data is published (KVKK
art. 10), so it goes on SkyMail's `islem` stream, reads no preference and
carries no unsubscribe link. Nothing promotional goes into it.

Run inside the core container (working directory `/app`, the binary is
not on `PATH`), after the template is seeded and
`TEAM_PAGE_CONSENT_GRACE_UNTIL` is set:

```
./core-backend team-page-notice           # dry run: counts, sends nothing, records nothing
./core-backend team-page-notice -apply    # sends
```

- **Recipients:** everyone a public roster lists (a `public_listing` team's
  members through every Group under it, a `public_leaders` team's leaders;
  nobody erased or being erased) without a team page decision, written to
  at Keycloak's primary e-mail; nobody disabled or without an address. One
  mail per person, naming every roster that lists them.
- **Template:** SkyMail `core.team-page-consent-notice` (skymail-frontend
  `emails/core-team-page-consent-notice.tsx`; `SKYMAIL_TEAM_PAGE_NOTICE_TEMPLATE_KEY`
  replaces the key). Variables: `FirstName`, `Teams` ("WEBLAB · Web
  Geliştirme, AGC"), `HideAt` (the transition's end, Europe/Istanbul,
  "11.11.2026 00:00"), `SettingsURL` (Account Center's Kulüp profili page:
  `COMMUNICATION_PREFERENCE_ACCOUNT_CENTER_URL` + `/club-profile`) and
  `Board` (`true` for a person on the Board's own roster, who stays there by
  first and last name; the mail then says so).
- **Once:** the ledger `team_page_notices` (migration `20261030100000`: the
  person's id and when) is written before each mail. A second run mails
  nobody it holds. A mail SkyMail refused is taken out of the ledger and
  goes on the next run; one sent without an answer stays in it and is never
  sent again (SkyMail's `/single` reads no idempotency key). A run stopped
  (Ctrl-C, SIGTERM) before a mail reached SkyMail takes that person's row
  out too. A Group list Keycloak read with a branch missing stops the run
  before anything is sent (exit 1, run it again).
- **Keeping:** a row is the proof the person was told: only their id and
  when. It is kept until the account is erased (account erasure deletes it);
  down refuses while the table holds one.
- **Running it again:** safe at any time during the transition. It mails
  nobody the ledger holds, so a later `-apply` (say on the 7th day) tells
  only who was added to a listed team since, or who could not be written to
  before; who decided since is left out like the first time.
- **Refusals:** no `TEAM_PAGE_CONSENT_GRACE_UNTIL`, or one that has passed,
  exits 2; consents off (`CONTACT_CONSENT_KEY` unset: nobody could consent)
  or a template SkyMail does not know exits 3; Keycloak (an incomplete Group
  list too), the database or a refused mail exits 1 (run it again). The record printed is counts only,
  never a person.
