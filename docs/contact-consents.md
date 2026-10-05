# Contact consents

A contact consent (Davet onayı, ADR-0062 in `skylab-kulubu/e-skylab`) is a
person's explicit, optional consent (KVKK art. 5/1) for one purpose:

| Purpose | What it allows | Given through | Taken |
|---|---|---|---|
| `event_invitations` | invitations to SKY LAB's future events by e-mail | Guest apply, Forms, Place, Guessr, a signed-in person | yes |
| `recruitment_pool` | keeping a team application that was not accepted, to be considered in future recruitment | Forms only | **no**: every route answers `400 purpose_not_enabled` until its scope (an address, or one application) is agreed with Forms. The table already allows the value |

Core keeps the single record (`contact_consents`). The products write to it;
SkyMail reads its invitation list only from it. Member and alumni
announcements are not contact consents: they run on legitimate interest with
their own opt-out.

The rules every product follows (ADR-0062): the box is **unticked**, separate
from the aydınlatma text and never a condition of the service; staff never
tick it for someone else; past guests are not mailed to ask for consent.

**Release:** keep `CONTACT_CONSENT_KEY` unset in production until the
periodic destruction run (the retention sweep, a separate change) deletes
unconfirmed, ended and unrenewed grants on time. Without the key nothing is
recorded.

## Grants

One row is one grant. It is recorded, confirmed, and ends; it is never
reopened. A new grant after an end is a new row, so the rows are the history
and the proof.

| State | Meaning |
|---|---|
| `pending` | recorded for an address nobody has shown to be the person's; waits for the person to confirm it from the link core mails (double opt-in). In no audience |
| `active` | confirmed: the person may be contacted for the purpose |
| `withdrawn` | the person withdrew it (a link, their account, or the product on their word) |
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
withdraw link still ends the new row (same address).

**The confirmation mail** goes once, when the pending grant is recorded. The
same grant given again (the same address and purpose, still pending) mails
it again at most once a day and three times in all (`confirmation_mails`):
whoever types an address cannot use core to flood it. The mail greets nobody
by name: whoever typed the address may have typed any name with it.

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

**Evidence.** Purpose, channel (`email`), the text version the person was
shown, the source app (`guest_apply`, `forms`, `place`, `guessr`, `self`), the
Keycloak client of the app (`client_id`), the Event it was given on (Guest
apply), how the address was shown to be the person's (`confirmed_via`:
`account`, `service`, `link`), and the times: granted, confirmed, renewed,
renewal asked, ended, with the end's reason and way (`ended_via`: `link`,
`one_click`, `self`, `service`, `renewal_unanswered`). No IP address, user
agent or name is kept.

**Texts.** A grant names the text version it was given on. Core accepts only
the versions below; a new wording is a new version here and in
`internal/consent` (`purposes`, `texts`), released before the products show
it. The codes are the versions ("Sürüm") of the Açık Rıza Metni draft
(sky_lab_genel `notes/hukuki/acik-riza-metni-davet-TASLAK.md`):

| Version | Purpose | Box text (Turkish, as shown next to the box, with links to the Açık Rıza Metni and the aydınlatma) |
|---|---|---|
| `davet-v1` | `event_invitations` | SKY LAB'ın gelecek etkinliklerine davet e-postası almak istiyorum. Bunun için adımı ve e-posta adresimi saklayabilirsiniz. İstediğim zaman her davetteki bağlantıyla vazgeçebilirim. |
| `alim-havuzu-v1` | `recruitment_pool` (not taken yet) | Bu dönem kabul edilmezsem başvurumun gelecek alımlarda değerlendirilmek üzere saklanmasını istiyorum. İstediğim zaman vazgeçebilirim. |

The draft is not approved yet; if the approved wording differs, it gets a new
version (`davet-v2`, …) here first. A grant that names no version is recorded
on the newest one of its purpose.

## Lifecycle

| When | What happens |
|---|---|
| 30 days pending | the confirm link stops working; the retention sweep deletes the row |
| 3 years without a confirmation, a renewal or a check-in at an Event | the grant is due its renewal question: the audience marks it `renewalDue` with a `renewUrl` |
| SkyMail sent the renewal question | `POST /v1/consents/renewal-requests` records it (`renewalRequestedAt`) |
| 60 days after the question without a renewal or check-in | the retention sweep ends the grant as `expired` |
| ended (withdrawn, expired or superseded) | the address is cleared at once; the retention sweep deletes the row 3 years later |
| account erasure | every row of the person is deleted, open or ended: their account's and those given for any of their addresses, Keycloak's included (step `erase_contact_consents`, right after the logout and before the services, so no invitation goes out while a service holds the saga); there is no suppression list (ADR-0051 decision 4) |

The rows marked "retention sweep" are the periodic destruction run's
(ADR-0062), a separate change; it reads `consent.RenewalAnchorSQL`,
`PendingTTL`, `RenewalAnswerWindow` and `ProofRetention`. Until it runs, a
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
| withdraw | `/v1/consents/withdraw?token=…` | ends every open grant of the link's purpose that mails the same address | never |
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

- `GET /v1/consents/audience?purpose=event_invitations&after={id}&limit={1..1000}` →
  `200 {"items": [...], "next": "{id}"|null}`. Each item:
  `{"id", "email", "subject": "account"|"address", "confirmedAt", "withdrawUrl", "renewalDue", "renewalRequestedAt"?, "renewUrl"?}`.
  Only confirmed, open grants, in id order. The same address can appear twice
  (a member's own grant and one given for the address): send one mail per
  address. Put `withdrawUrl` in the footer and the `List-Unsubscribe` header.
  An item with `renewalDue` gets the renewal question (with `renewUrl` and
  `withdrawUrl`) instead of an invitation, **once**: an item whose
  `renewalRequestedAt` is set has been asked; do not ask again, and send it
  no invitation while it waits.
- `POST /v1/consents/renewal-requests` `{"purpose", "ids": [≤ 1000]}` →
  `200 {"requested": n}`: the renewal question went to these grants. Only
  due, not yet asked grants change.

### A signed-in person

- `GET /v1/users/me/consents` → `{"items": [{"id", "purpose", "status", "subject", "source", "textVersion", "grantedAt", "confirmedAt"?, "endedAt"?}]}`:
  their account's grants and those given for any of their addresses: every
  address Keycloak holds (Primary, School and Personal e-mail) with core's
  row, read as account erasure reads them. Keycloak unreachable is an error,
  not a shorter list.
- `POST /v1/users/me/consents` `{"purpose", "textVersion"?}` → `201`/`200 {"status":"active"}`.
- `DELETE /v1/users/me/consents/{purpose}` → `200 {"withdrawn": n}`: their
  account's grant and every grant that mails one of those addresses.

A service account cannot use these.

## Settings

| Variable | |
|---|---|
| `CONTACT_CONSENT_KEY` | 32 random bytes, unpadded base64url (OpenBao reference). Unset: off. Keep it unset in production until the retention sweep ships. Never rotate: mailed links and proof keys depend on it |
| `PUBLIC_API_ORIGIN` | required with the key: where the links point |
| `CONTACT_CONSENT_TEXT_URL` | required with the key: the published Açık Rıza Metni the confirm page links. There is no published one yet (the draft is under legal review), so the key cannot be set before it is |
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
