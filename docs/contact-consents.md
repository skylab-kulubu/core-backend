# Contact consents

A contact consent (Davet onayı, ADR-0062 in `skylab-kulubu/e-skylab`) is a
person's explicit, optional consent (KVKK art. 5/1) for one purpose:

| Purpose | What it allows | Given through | Taken |
|---|---|---|---|
| `event_invitations` | invitations to SKY LAB's other events and its announcements by e-mail (aydınlatma s6 row 14, §5.14; `davet-v2`) | Guest apply, Forms, Place, Guessr, a signed-in person | yes |
| `promotional` | mails whose main purpose is promoting a product or a service: a discount code, a company's job posting, a sale, a raffle (aydınlatma s6 row 13 b, §5.8, §5.10). A short sponsor thank-you or logo in an announcement is not promotional and needs no consent | SkyForms forms (`forms`), the first Account Center screen after signup (`signup`), Account Center's communication preferences (`self`) | yes |
| `recruitment_pool` | keeping a team application that was not accepted, to be considered in future recruitment | Forms only | **no**: every route answers `400 purpose_not_enabled` until its scope (an address, or one application) is agreed with Forms. The table already allows the value |
| `team_page` | being shown on one's teams' public pages (2026-10-27 to 2026-10-31). Since then every member is shown unless they object, so it is no longer taken; its rows are kept as proof ([`team-page-consent.md`](team-page-consent.md)) | — | **no**: `400 purpose_not_enabled` |
| `team_page_objection` | not an allowance: an objection to being shown on one's teams' public pages (aydınlatma s6 row 4 b, legitimate interest; [`team-page-consent.md`](team-page-consent.md)). An open grant hides the person; withdrawing it shows them again. Channel `web`, no mail route serves it | the person, for their own account, in Account Center (`self`) | yes |

Core keeps the single record (`contact_consents`). The products write to it;
SkyMail reads its invitation and promotional lists only from it. Member and alumni
announcements are not contact consents: they run on legitimate interest with
their own opt-out, the communication preference
([`communication-preferences.md`](communication-preferences.md)).

The rules every product follows (ADR-0062): the box is **unticked**, separate
from the aydınlatma text and never a condition of the service; staff never
tick it for someone else; past guests are not mailed to ask for consent.

**Release:** keep `CONTACT_CONSENT_KEY` unset in production until the
periodic destruction run ([`retention-sweep.md`](retention-sweep.md)) runs in
apply mode, so that it deletes unconfirmed and ended grants and ends
unrenewed ones on time. Without the key nothing is recorded.

## Grants

One row is one grant. It is recorded, confirmed, and ends; it is never
reopened. A new grant after an end is a new row, so the rows are the history
and the proof.

| State | Meaning |
|---|---|
| `pending` | recorded for an address nobody has shown to be the person's; waits for the person to confirm it from the link core mails (double opt-in). In no audience |
| `active` | confirmed: the person may be contacted for the purpose |
| `withdrawn` | the person withdrew it (a link, their account, the product on their word, or staff on their mail) |
| `expired` | the renewal question went unanswered |
| `superseded` | a pending grant that a verified grant for the same address replaced (below) |

**Who confirms.** A grant is active at once when the person is signed in and
consents for their own account, or when the calling product verified the
address (`emailVerified`: a code sent to it, a verified sign-in) **and** its
client is one of `CONTACT_CONSENT_VERIFIED_CLIENTS` (default: none). Every
other grant is `pending` until the person opens the confirmation mail and
presses the button: every Guest apply one, and a product's whose word core
has not been told to take. Guest apply is public, so anybody may type
anybody's address, and an operator adding a guest cannot consent for them:
the confirmation link is what lets the person decide themselves.

**A verified grant meeting a pending one** does not inherit its evidence. The
pending row ends as `superseded` (its own source, client, Event and text
stay as they were, its address goes), and the verified request is a new row
with its own source, client, Event, text and `confirmed_via = service`. The
pending row's confirm link then says the consent is already recorded; its
withdraw link still ends the new row (same address). The superseded row was
never consent, so it is no proof: the retention sweep deletes it on the
pending schedule (30 days after its last confirmation mail), as it does a
grant withdrawn while still pending, and from then on its links find
nothing; every invitation carries the new row's own withdraw link.

**The confirmation mail** goes once, when the pending grant is recorded. The
same grant given again (the same address and purpose, still pending) mails
it again at most once a day and three times in all (`confirmation_mails`):
whoever types an address cannot use core to flood it. The mail greets nobody
by name: whoever typed the address may have typed any name with it.

**Known behaviour: the cap is per grant.** Ticking the box, withdrawing,
and ticking it again in a product records a new pending grant each time,
since a grant is never reopened, and each new one starts its own three
mails. Core does not count across grants: a withdrawn pending grant is
inert (in no audience) and the person may change their mind. What bounds
the loop is the product that records the grants: its own rate limits and
sign-in (Guest apply: one Ticket per address and Event, and its request
limits; Forms, Place and Guessr through their service accounts). A product
that lets a visitor toggle the box without such limits should not record
each toggle.

**The subject** is exactly one of:

- a core user (`user_id`): a signed-in person consenting for themselves. The
  address is read from `users.email` when it is needed, so it follows an
  address change and no copy is kept;
- an address (`email`): the normalized (trimmed, lower-case) address, kept
  only while the grant is open. `email_hmac`, its keyed HMAC-SHA256, stays
  with the row as the proof key after the address is gone.

The address of an open grant is kept in clear, not encrypted: invitations
are sent to it, the retention sweep must match it against `tickets.guest_email`
(which holds the same address in clear for as long as the grant is open), and
an encryption key lost or rotated would lose the audience. A grant given for
an address is found by its HMAC, and an ended one keeps only the HMAC: a plain
hash of an address is reversible by guessing addresses, the keyed one is not
without `CONTACT_CONSENT_KEY`.

**A mailbox, not a row.** A person's own grant mails their account's address,
and a grant given for that address mails the same mailbox. Every way of
withdrawing (a link, the product, the person's account) therefore ends every
open grant of the purpose that mails the address: the grants given for it and
the own grant of an account whose `email` or `school_email` it is. That holds
for a link from an old invitation too, whose address grant has since ended
and kept only its HMAC. One click ends the mails, whichever grant a mail was
sent on.

**A person, not a row (withdraw links).** A person holds one grant of a
purpose, even when two rows record it: their account's own and one a product
recorded for another of their addresses, such as the Personal e-mail that
only Keycloak holds. A withdraw link therefore ends all of them:

- the link of an account's own grant ends it and every grant given for one
  of the account's addresses, Keycloak's Primary, School and Personal e-mail
  with core's row (as their own list reads them);
- the link of a grant given for an address ends every grant given for that
  address and the own grant of every account holding it (core's `email` or
  `school_email`, or Keycloak's Personal e-mail,
  `GET /users?q=personalEmail:…`). A grant for another address of that
  account mails another mailbox and is left to its own link. An ended
  address grant keeps only its HMAC, so its link then reaches accounts by
  core's row alone.

Keycloak unreachable never stops a withdrawal: the link ends what core
reaches without it (the grant's own subject and core's row) and logs the
rest without an address; the person's account (below) reaches the others.

**Evidence.** Purpose, channel (`email`), the text version the person was
shown, the source app (`guest_apply`, `forms`, `place`, `guessr`, `self`,
`signup`), the
Keycloak client of the app (`client_id`), the Event it was given on (Guest
apply), how the address was shown to be the person's (`confirmed_via`:
`account`, `service`, `link`), and the times: granted, confirmed, renewed,
renewal asked, ended, with the end's reason and way (`ended_via`: `link`,
`one_click`, `self`, `service`, `renewal_unanswered`, `staff`). No IP address, user
agent or name is kept.

**Texts.** A grant names the text version it was given on. Core accepts only
the versions below; a new wording is a new version here and in
`internal/consent` (`purposes`, `texts`), released before the products show
it. The codes are the versions ("Sürüm") of the Açık Rıza Metni, published
at <https://yildizskylab.com/acik-riza-metni.pdf> (source: sky_lab_genel
`notes/hukuki/acik-riza-metni-YAYIN-2026-10-25.md`, from the approved draft
`notes/hukuki/acik-riza-metni-20261024-taslak.md`):

| Version | Purpose | Box text (Turkish, as shown next to the box, with links to the Açık Rıza Metni and the aydınlatma) |
|---|---|---|
| `davet-v2` | `event_invitations` | Diğer etkinlik ve duyurulardan haberdar olmak istiyorum. |
| `tanitim-v1` | `promotional` | SKY LAB'dan tanıtım içerikli e-postalar (indirim kodu, şirket iş ilanı, çekiliş gibi) almak istiyorum. |
| `ekip-sayfasi-v1` | `team_page` (no longer taken) | Ekip sayfalarında görüneyim: adım, soyadım, profil fotoğrafım, LinkedIn adresim, üniversitem, fakültem, bölümüm ve ekip liderliğim ekibimin SKY LAB uygulamasındaki sayfasında yayımlansın. |
| `ekip-sayfasi-itiraz-v1` | `team_page_objection` | Account Center's switch, which the person turned off (not a box, no Açık Rıza Metni): Ekip sayfalarında görün. Adın, soyadın, profil fotoğrafın, LinkedIn adresin, üniversiten, fakülten, bölümün ve ekip liderliğin ekibinin SKY LAB uygulamasındaki sayfasında yer alır. |
| `alim-havuzu-v1` | `recruitment_pool` (not taken yet) | Bu dönem kabul edilmezsem başvurumun gelecek alımlarda değerlendirilmek üzere saklanmasını istiyorum. İstediğim zaman vazgeçebilirim. |

A changed wording is a new version here and in a new edition of the
published text. `alim-havuzu-v1` is not in the published text; it gets there
before its purpose is enabled. `ekip-sayfasi-v1` is not in it either: the
switch shows the whole text where the consent is taken (aydınlatma s6 §6),
and a new edition of the published text may add it. A grant that names no version is recorded on
the newest one of its purpose. `davet-v1` (the invitations alone, from an
earlier draft that was never published) is no longer taken: a grant naming it
answers `400`. No production grant names it (`CONTACT_CONSENT_KEY` is unset
there and the table is empty); core keeps its box text so a grant stored on it elsewhere
still shows it, and SkyMail reads the version of each grant in the audience
(`textVersion`): a `davet-v1` grant is for invitations, not for
announcements.

**The promotional consent (aydınlatma s6 §5.8).** One grant per person
and mailbox, whichever box gave it: the form, the signup screen or the
preferences. A product shows the box only while `lookup` (or the person's
own list) has no open grant for the address, and an unticked box records
nothing and withdraws nothing. A grant given in a form without an account is
bound to the address typed there and is `pending` until the person confirms
it from the mail core sends (double opt-in); once the person opens an
account with that address the grant is the account's, as the mailbox rule
above says: their own list shows it, `lookup` counts it, and withdrawing
from their account, a promotional mail's withdraw link or the product ends
it. The row is not rewritten to the account: it stays the proof of what the
form recorded. A box ticked all the same (the person's Personal e-mail in a
form, which `lookup` does not see; the signup screen after a form grant)
records a second row; any withdrawal still ends both. A withdrawal asked by
mail to info@yildizskylab.com is staff's ("Withdrawal by mail", below). An
ended grant is kept 3 years as proof (`consent_proof`), then deleted.

**Withdrawal by mail (aydınlatma s6 §6).** Every consent may also be
withdrawn by writing to info@yildizskylab.com. Staff carry it out on the
server, inside the core container, with the address on standard input so
that it is in no argument, process list, shell history or log:

```
printf '%s\n' "$address" | docker exec -i <core> ./core-backend consent-withdraw -purpose promotional,event_invitations
```

sky_lab_genel `ops/wizards/kvkk-riza-geri-alma-wizard.sh` runs it from the
Mac (the address typed hidden, sent over SSH's standard input). It ends, for
the purposes named, every open grant of the person holding the address: the
grants given for the address, and the own grant of every account holding it
(core's row, or Keycloak's Personal e-mail) with the grants for every other
address of that account, which a withdraw link leaves to their own links. Unlike a link it
reads every address first: Keycloak unreachable ends nothing and exits 1, to
be run again. Each grant it ends says so (`ended_via` `staff`); that row,
kept as proof like any ended grant, is the record of the request. The
command prints the time and how many grants ended per purpose, never the
address; running it again ends none. Exit codes: `0` done, `1` core or
Keycloak failed (run it again; what ended stays ended), `2` usage or
configuration, `3` consents off (`CONTACT_CONSENT_KEY` unset: none is
recorded).

## Lifecycle

The renewal rows apply to `event_invitations` only. A `promotional` grant
lasts until it is withdrawn: it is never due a renewal question, its
audience items never carry `renewalDue`, and a renewal request for it changes
nothing, so it never ends as `expired`.

| When | What happens |
|---|---|
| 30 days after the last confirmation mail of a grant never confirmed (pending, superseded, or withdrawn while pending) | the confirm link stops working; the retention sweep deletes the row (it was never consent, so it is no proof) |
| 3 years without a confirmation, a renewal or a check-in at an Event | the grant is due its renewal question: the audience marks it `renewalDue` with a `renewUrl` |
| SkyMail sent the renewal question | `POST /v1/consents/renewal-requests` records it (`renewalRequestedAt`) |
| 60 days after the question without a renewal or check-in | the retention sweep ends the grant as `expired` |
| a check-in after the question | answers it as the link would: the question is void (`consent.RenewalQuestionSQL`), the audience no longer shows it, the grant never expires on it, and 3 years after that check-in the grant is due and asked again |
| ended (withdrawn, expired or superseded) | the address is cleared at once; the retention sweep deletes a grant that was once confirmed 3 years after its end (one never confirmed on the pending schedule, above) |
| account erasure | every row of the person is deleted, open or ended: their account's and those given for any of their addresses, Keycloak's included (step `erase_contact_consents`, right after the logout and before the services, so no invitation goes out while a service holds the saga); there is no suppression list (ADR-0051 decision 4) |

The rows marked "retention sweep" are the periodic destruction run's
(ADR-0062, [`retention-sweep.md`](retention-sweep.md): `consent_pending`,
`consent_renewal_unanswered`, `consent_proof`); it reads
`consent.RenewalAnchorSQL`, `PendingTTL`, `RenewalAnswerWindow` and
`ProofRetention`. A note on `consent_renewal_unanswered`'s version is in
[`retention-sweep.md`](retention-sweep.md#known-behaviours). Until it runs, a
pending grant whose link has expired stays inert (in no audience, its link
refused) and an ended grant keeps only its HMAC. No page promises a deletion
before the sweep runs.

A check-in is the attendance: the person's own Tickets, or the guest Tickets
of the address.

**Against the proposal's column list** (`.scratch/data-lifecycle/saklama-sureleri-onerisi.md`
§7.2): `email_normalized` is `email` (normalized, open grants only) plus
`email_hmac`; `given_at` is `granted_at`; `withdrawn_at` is `ended_at` with
`ended_reason`; `ended_reason` has no `account_erased`, because erasure
deletes the rows (ADR-0062); `last_event_at` is not stored but read from the
check-ins when it is needed (`RenewalAnchorSQL`), so it cannot drift; source
`account_center` is `self` with the app's client in `client_id`. Added:
`user_id` (a person's own grant), `confirmed_at`/`confirmed_via` (double
opt-in), `confirmation_sent_at`/`confirmation_mails` (the mail cap),
`renewed_at`, `renewal_requested_at`, `ended_via`, `event_id`, and the end
reason `superseded`.

## Links

Two signed links, opened without a sign-in. The token is the permission: an
HMAC-signed grant id (no address, no person) that only core can read.

| Link | Path | Does | Expires |
|---|---|---|---|
| withdraw | `/v1/consents/withdraw?token=…` | ends every open grant of the link's purpose of the person the grant mails (above) | never |
| confirm | `/v1/consents/confirm?token=…` | confirms a pending grant, renews an active one; does nothing for an ended one | 30 days (confirmation), 90 days (renewal) |

`GET` shows a page with one button and changes nothing, so a mail scanner that
opens the link does not act. The page names exactly what the link is for: the
purpose of the grant it names, and on the confirm page the box text of that
grant's version, with links to the Açık Rıza Metni (`CONTACT_CONSENT_TEXT_URL`)
and the KVKK aydınlatma metni (`CONTACT_CONSENT_NOTICE_URL`). The button
`POST`s the token in the form body. A withdraw link withdraws what is open
*now* for the mailbox, so a link from an old invitation still works after the
person granted again. Withdrawing twice answers as once.

**One-click unsubscribe (RFC 8058).** Every mail sent on a grant carries:

```
List-Unsubscribe: <https://api.yildizskylab.com/v1/consents/withdraw?token=…>
List-Unsubscribe-Post: List-Unsubscribe=One-Click
```

The mail client `POST`s `List-Unsubscribe=One-Click` (form-encoded or
multipart) to that address; core withdraws and answers `200` with no
redirect. The header must be covered by the mail's DKIM signature.

The pages answer with `Cache-Control: no-store`, `Referrer-Policy: no-referrer`,
`X-Frame-Options: DENY` and a CSP without scripts. Each client address may
open the pages (and confirm) 120 times a minute, and withdraw (`POST` of the
withdraw link, one-click included) 600 times a minute in a budget of its own:
a mail provider's one-click POSTs come from a few of its addresses for all
its users. A forged token costs one HMAC.

## API

All bodies are JSON. Errors are `application/problem+json` with a `code`:
`consent_invalid` (400), `consent_text_unknown` (400), `purpose_not_enabled`
(400), `consent_role_missing` (403), `consent_source_unknown` (403),
`consent_person_only` (403), `consents_unavailable` (503,
`CONTACT_CONSENT_KEY` unset).

### Guest apply

`POST /v1/events/{eventId}/applications/guest` takes an optional `consents`
field ([guest-apply.md](guest-apply.md)): a list of purpose names or of
`{"purpose", "textVersion"}` objects. Absent or empty records nothing. Only
`event_invitations` may be given here, at most once (a repeat with the same
text counts once; more entries than purposes, or one purpose with two texts,
is `400`). The whole field, the guest's address included, is judged before
anything is written: an address a consent cannot use (`ada@localhost`, `ada`,
`a,b@example.com`), an unknown purpose or text is `400` and no Ticket is
written. After the Ticket each grant is recorded `pending` for the guest's
address and core mails the confirmation; the answer does not change. A grant
that cannot be recorded for a passing reason (the database) turns the answer
into `503` (`consent_not_recorded`, `Retry-After: 1`): the caller sends the
application again, which finds the Ticket and records the grant.

### Products: role `consent:record`

A product's service account (client credentials) holding the `consent:record`
role of the `core` client, whose client is mapped to a source in
`CONTACT_CONSENT_SERVICE_CLIENTS` (default `forms:forms,place:place,guessr:guessr`).

- `POST /v1/consents` `{"purpose", "textVersion"?, "email", "emailVerified"?}`
  → `201 {"status":"pending"|"active"}` for a new grant, `200` when an open
  one was there and stays. `emailVerified` counts only from a client of
  `CONTACT_CONSENT_VERIFIED_CLIENTS`; any other grant answers `pending`. A
  verified grant meeting a pending one answers `201 active` (a new row, the
  pending one superseded). An unverified grant meeting a pending one answers
  `200 pending` and mails the confirmation again only under the cap above.
- `POST /v1/consents/withdrawals` `{"purpose", "email"}` → `200 {"withdrawn": bool}`:
  the person unticked the box in the product. Every open grant that mails the
  address ends.
- `POST /v1/consents/lookup` `{"purpose", "emails": [≤ 500]}` →
  `200 {"states": {"a@b.c": "active"|"pending"}}`, the addresses normalized;
  an address no open grant mails is absent. It counts the grants given for
  the address and the own grant of an active account with it. Place and
  Guessr read it for whether a player's address may be kept past the event
  (ADR-0062). It runs as one query of equality joins (500 addresses take a
  few milliseconds at 3,000 accounts and grants) and stops after ten
  seconds.

### SkyMail: role `consent:audience:read`

- `GET /v1/consents/audience?purpose=event_invitations|promotional&after={id}&limit={1..1000}` →
  `200 {"items": [...], "next": "{id}"|null}`. Each item:
  `{"id", "email", "subject": "account"|"address", "confirmedAt", "textVersion", "withdrawUrl", "renewalDue", "renewalRequestedAt"?, "renewUrl"?}`.
  Only confirmed, open grants, in id order. The same address can appear twice
  (a member's own grant and one given for the address): send one mail per
  address. Put `withdrawUrl` in the footer and the `List-Unsubscribe` header.
  An item with `renewalDue` gets the renewal question (with `renewUrl` and
  `withdrawUrl`) instead of an invitation, **once**: an item whose
  `renewalRequestedAt` is set has been asked; do not ask again, and send it
  no invitation while it waits. `renewalRequestedAt` is the standing
  question only: one the person answered by coming to an Event is no longer
  shown, and the item is asked again when it is next due.
- `POST /v1/consents/renewal-requests` `{"purpose", "ids": [≤ 1000]}` →
  `200 {"requested": n}`: the renewal question went to these grants. Only
  due grants without a standing question change.

### A signed-in person

- `GET /v1/users/me/consents` → `{"items": [{"id", "purpose", "status", "subject", "source", "textVersion", "grantedAt", "confirmedAt"?, "endedAt"?}]}`:
  their account's grants and those given for any of their addresses: every
  address Keycloak holds (Primary, School and Personal e-mail) with core's
  row, read as account erasure reads them but without erasure's limit of
  three (core's row may still hold an older Primary). Keycloak unreachable is
  `503` `consent_addresses_unavailable` with `Retry-After`, not a shorter
  list.
- `POST /v1/users/me/consents` `{"purpose", "textVersion"?, "source"?}` → `201`/`200 {"status":"active"}`.
  `source` is where the person ticked the box: `self` (unset: Account
  Center's preferences, Place, Guessr) or `signup` (the first Account Center
  screen after the account was opened; `promotional` only). Anything else is
  `400 consent_invalid`.
- `DELETE /v1/users/me/consents/{purpose}` → `200 {"withdrawn": n}`: their
  account's grant and every grant that mails one of those addresses. While
  Keycloak is unreachable it ends at once what core reaches without it (the
  account's grant and the grants for core's row's addresses) and answers
  `503` `consent_addresses_unavailable` with `Retry-After`: a grant for
  another of their addresses (a Personal e-mail) may still be open, so the
  page asks them to try again.

A service account cannot use these.

## Settings

| Variable | |
|---|---|
| `CONTACT_CONSENT_KEY` | 32 random bytes, unpadded base64url (OpenBao reference). Unset: off. Keep it unset in production until the retention sweep ships. Never rotate: mailed links and proof keys depend on it |
| `PUBLIC_API_ORIGIN` | required with the key: where the links point |
| `CONTACT_CONSENT_TEXT_URL` | required with the key: the published Açık Rıza Metni the confirm page links: `https://yildizskylab.com/acik-riza-metni.pdf`. The key is not set before that address answers 200 |
| `CONTACT_CONSENT_NOTICE_URL` | the KVKK aydınlatma metni the pages link; unset: `https://yildizskylab.com/kvkk-metni.pdf` (the published one) |
| `CONTACT_CONSENT_SERVICE_CLIENTS` | `source:client` pairs; `none` for none |
| `CONTACT_CONSENT_VERIFIED_CLIENTS` | clients of the line above whose `emailVerified` core believes; unset: none |
| `SKYMAIL_CONSENT_CONFIRM_TEMPLATE_KEY` | the confirmation mail's SkyMail template, default `core.contact-consent-confirm`; variables `confirmUrl`, `withdrawUrl`, `purpose` |

The confirmation mail goes through core's SkyMail client (`SKYMAIL_URL` and
core's Keycloak client). Without it a grant stays pending; startup logs
`contact consents: on|off`.

Keycloak (follows in e-skylab-keycloak): the `core` client roles
`consent:record` (Forms, Place, Guessr service accounts) and
`consent:audience:read` (SkyMail's service account), each granted to the
service account only, and each of those clients' tokens must carry core's
roles (`resource_access.core.roles`, audience `core`), as Forms' does for
`media:attach`. A person never holds either role; a person's token is refused
on these routes whatever it carries.

The confirmation mail's template is seeded in SkyMail by key
([skymail-templates.md](skymail-templates.md)); until it is, SkyMail refuses
the send (`skymail_call_failed`, `kind=consent_confirmation`) and Guest apply
grants stay pending.
