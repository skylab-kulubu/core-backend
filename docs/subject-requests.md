# KVKK md. 11 data subject requests

The board (YK) keeps the register of the requests people make about their personal data (KVKK md. 11) in core: each request that reaches `info@yildizskylab.com`, the post or a hand is recorded in the admin panel ("KVKK başvuruları"), with when and how it was answered. The answer is due within 30 days (KVKK md. 13/2; Kurul's Veri Sorumlusuna Başvuru Usul ve Esasları Hakkında Tebliğ md. 6), and the register is what proves it. Decision: Yusuf, 2026-10-27 (hub `.scratch/kvkk-basvuru-kaydi`); the procedure is the hub's `notes/hukuki/yk-info-kutusu-yordami-20261027.md`.

## What a request holds

The least data that proves the request and its answer. **Never** an ID number (T.C. kimlik no, passport), an address of residence beyond where the answer goes, a verification code or a document: those stay in the mailbox, and a copy of an ID document is deleted once checked.

| Field | |
|---|---|
| `receivedOn` | the day it reached the club (`YYYY-MM-DD`), not after today in Turkey; when entered or changed, not more than a year before today (a mistyped year would otherwise let the sweep delete a request too early). An older request can still be closed |
| `channel` | `email` (info@), `post`, `in_person` |
| `requesterName` | the requester's name, 1–200 characters |
| `requesterContact` | where the answer goes (an e-mail or a postal address), 1–320 characters |
| `userId` | the account, when the requester has one; `null` otherwise. Account erasure anonymizes the account's row, so `userId` then names the anonymized row (no personal data in it); a row deleted outright would empty it. The request stays either way. A request may name an account that is being erased (its erasure request is itself one) |
| `requestTypes` | at least one, kept once each in the law's order: `learn` (md. 11/1-a), `info` (b), `purpose` (c), `recipients` (ç), `rectify` (d), `erase` (e), `notify_recipients` (f), `object_automated` (g), `compensation` (ğ), `other` (a consent withdrawal or a photo objection that came the same way) |
| `status` | `received`, `awaiting_verification` (a person without an account has not yet confirmed the address), `answered`, `rejected` (a reasoned refusal). The last two are concluded |
| `dueOn` | `receivedOn` + 30 days, computed by the database |
| `answeredOn` | the day the answer went; required when concluded, empty otherwise, not before `receivedOn`, not after today. Moving a request back to an open status empties it |
| `answerSummary` | a short summary of the answer, at most 2,000 characters |
| `responder` | who answered for the club, by title; `YK Başkanı` when left empty (on create and on change), at most 100 characters |
| `createdAt`, `createdBy`, `updatedAt`, `updatedBy` | when and by whom (Keycloak subject) |

There are no attachments and no correspondence store: the mail stays in info@.

## Routes

All four need `subject-request:manage` as the role mode reads it ([authz-roles.md](authz-roles.md)): in the `groups` mode a member of ADMIN or YK (or a subgroup). **Every action, a read too, is decided as a write**: DK (a Privileged reader) and `global:read` get 403 in every mode, and so does a service account. Every answer is `Cache-Control: no-store`.

| Route | |
|---|---|
| `GET /v1/subject-requests` | the requests: open ones first, soonest due first; then the concluded, latest answered first; at most 500 |
| `POST /v1/subject-requests` | records one; `201` with it. `status` defaults to `received` |
| `GET /v1/subject-requests/{id}` | one request with its `events` (the trail, oldest first) |
| `PATCH /v1/subject-requests/{id}` | writes the fields the body sends; a field left out keeps its value, `null` empties `userId` and `answeredOn`. With `updatedAt` (the value last read) it is refused with `409` `subject_request_changed` when someone changed the request since |

`401` without a bearer, `403` for anyone else, `404` for an unknown id. A refused field is `400` with a `code`: `received_on_invalid`, `channel_invalid`, `requester_name_invalid`, `requester_contact_invalid`, `user_invalid` (not an account id), `user_unknown` (no such account), `request_types_invalid`, `status_invalid`, `answered_on_required`, `answered_on_invalid`, `answer_summary_invalid`, `responder_invalid`. There is no delete route: a request leaves the register only through the retention sweep.

The panel shows `can["subjectRequest.manage"]` of `GET /v1/users/me/capabilities` as its menu; the deadline badge ("N gün kaldı" within 7 days, "Süre geçti" past it) is the panel's, computed from `dueOn`. No mail is sent.

## Trail

`subject_request_events` keeps one row per creation and per change that changed something: when, who (Keycloak subject), `create` or `update`, the **names** of the fields written and the status after it. Never a value. Reads are not recorded. The trail goes with its request.

## Retention

Three years after the request was answered or rejected (ADR-0062, "KVKK md. 11 başvuruları ve yazışmaları"), the retention sweep's `subject_requests_age` rule deletes it with its trail ([retention-sweep.md](retention-sweep.md)). An open request is never deleted by it; the badge keeps it in sight. Account erasure keeps the request: the proof of the answer is a legal obligation, and it may itself be the erasure request.

## Keycloak

`subject-request:manage` is a new role of core's client. In the `groups` mode (production's today) nothing has to change in Keycloak. Before the `both` or `roles` mode, create it on `core` and map it to `/ADMIN`, `/UYELER/ADMIN`, `/YK` and `/UYELER/YK` only (e-skylab-keycloak); mapped to DK too, it still allows DK nothing.

## Compared with others

KVKK md. 11/13 and the Tebliğ: 30 days, free, written or electronic answer, a refusal with reasons. Matches: the deadline from receipt, the answer's date and summary, a reasoned refusal as its own status, the channels the club uses among the Tebliğ's (no KEP or e-signature here). OneTrust's DSAR register (and TrustArc, Microsoft Purview DSR) also keep a type, the subject, an identity check, a deadline, an assignee, a status and an audit trail. Deviations: no extension (KVKK has none, unlike GDPR's two months), no intake portal (info@ is entered by hand), no attachments or correspondence (least data), a fixed responder title instead of an assignee, no record of reads.
