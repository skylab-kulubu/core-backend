# Certificate mail queue

The mail that follows an issued certificate (`core.certificate`) goes through core's durable queue, `certificate_mails` (migration `20261125101500`). Before it, the mail was posted once, inline, and lost when SkyNotify did not answer; nothing could send it again short of a reissue (a new serial).

The send time does not change: an automatically closed attendance (an hour after the last live Session, [certificate-rule.md](certificate-rule.md)) issues the certificates, and each issued certificate queues its mail.

## Queue

- **Issued.** The issuance job writes one `issued` row for the certificate before it completes the job. A job claimed again after a crash finds its certificate and writes the row again, which the unique index `certificate_mails_issued_key` turns into nothing: one issued mail per certificate.
- **A row holds ids** (certificate, Event, who asked for a resend), never an address or a name. The worker reads the certificate's recipient when it sends; a certificate erased meanwhile takes its rows with it (`ON DELETE CASCADE`), one with no address fails as `no_recipient`.
- **Worker** `certificate mail` (`certificate.MaintainMails`): every 5 seconds, up to 5 due rows per claim (`MailBatch`). A claim holds its rows for every mail at its longest (reading its PDF, at most 10 s, then a token request and two posts, 3 × 15 s each) plus a minute, and counts the try (`claimed_until`, `claim_token`); the first try's time is kept (`first_tried_at`). Before each send the worker renews that row's lease for one send; a row whose lease ran out meanwhile is skipped, not sent, and the next claim takes it. Each send runs under a 45-second deadline (the PDF's read has its own 10 seconds before it). Only the lease holder settles a row.
- **Idempotency.** Each row goes to SkyNotify's `POST /v1/mail_tasks/single` under `Idempotency-Key: core-certificate-mail:<row id>`, the same on every try: SkyNotify answers a repeat with its first answer (it keeps a key 72 hours), so a try that got no answer is tried again without mailing twice. The id fallback (a key SkyNotify does not know, `SKYNOTIFY_CERTIFICATE_TEMPLATE_ID` set) goes under `<key>:id`.
- **Outcome.**

| SkyNotify | Row |
| --- | --- |
| 2xx | `sent`, `sent_at` |
| 400, 422 (the body is refused) | `failed`, `skynotify_<status>` |
| no answer, 5xx, 401, 403, 404, 409, 429 | queued again after 1, 2, 4 … 60 minutes, then hourly; `failed` once the next try would fall more than 24 hours after the first (`MailRetryWindow`), `skynotify_unreachable` or `skynotify_<status>` |
| first tried more than 48 hours ago (`MailKeyLimit`; a worker stopped for days) | `failed`, `expired`, not sent: SkyNotify keeps a key 72 hours, a later try could mail twice |
| no `SKYNOTIFY_URL`, no template | `failed`, `mail_unconfigured` |
| the certificate was revoked meanwhile | `cancelled`, `revoked` (nothing is sent) |

## PDF

`CERTIFICATE_PDF_ATTACHMENT=on` (unset or `off`: off, the mail as before) sends the certificate's PDF with its mail, the queued `issued` mail and every resend alike, the way `TICKET_PDF_ATTACHMENT` sends the Ticket's (SkyNotify README "Posta eki", ADR 0082).

- **The file** is the issued PDF itself, read before each send the way `GET /v1/certificates/{serial}/pdf` reads it (a fresh row; the artifact store's `certificates/<serial>.pdf`, or the row's own PDF for a certificate issued before the store), under 10 seconds (`MailPDFTimeout`). A certificate revoked between the claim and the send has no PDF any more (`read_failed`): that mail goes without the file. It goes as `SKY-LAB-sertifika-<serial>.pdf`, `application/pdf`: ASCII letters, digits and `-`, never the person's name.
- **Retries** read the same bytes again and post them under the same Idempotency-Key. SkyNotify keeps attachments out of the key's digest, so a try with the file and one without are the same mail: no `422`, no second mail.
- **Bounds.** SkyNotify takes at most 2 files, 512 KB decoded together (`mail.MaxAttachmentBytes`); base64 makes that about 683 KB of the request, far under Graph `sendMail`'s 4 MB. A template with a large background image can draw a PDF over it; that mail goes without the file (below).
- **Never a lost mail.** The mail goes without the file, with its verify link (`VerifyURL`, where the PDF is downloaded), and its outcome is what SkyNotify says to it, when: the PDF is over 512 KB (`too_large`), it cannot be read in time (`read_failed`), or SkyNotify refuses the file (`400 attachment.*`, `refused`; it is posted again at once without the file, under the same key, which a refused request leaves free). Any other refusal (`400`, `422` with another code) settles the mail as before. Each writes a `WARN certificate mail <row id>` line and counts `core_certificate_mail_pdf_skipped_total{reason="…"}`; a sent file counts `core_certificate_mail_pdf_attached_total` (`/v1/metrics`).
- **Off,** or a Mailer that takes no files, the body is what it was: no `attachments` field.

## Resend

For whoever may issue the Event's certificates (`certificate:issue`; else `403`):

- `POST /v1/certificates/{serial}/mail/resend`: one certificate, whatever its last mail did. `409` for a revoked certificate.
- `POST /v1/events/{id}/certificates/mail/resend` with `{"scope": "failed"}` (the active certificates whose latest mail failed) or `{"scope": "all"}` (every active certificate with an address). Another scope is `400`. The rows are written in one `INSERT … SELECT … ON CONFLICT DO NOTHING`. `all` runs at most once per 10 minutes for an Event (`certificate_mail_resend_all`, moved in the same transaction): a second one within it is `429` `resend_too_soon`. `failed` and the single resend have no cooldown; the queue's unique index keeps them from doubling a waiting mail.

Both write `resend` rows (`requested_by` is who asked, `resend_scope` `one`, `failed` or `all`) and answer `202` with `{"queued": n, "alreadyQueued": m}`: a certificate whose mail is waiting is not queued twice (`certificate_mails_pending_key`), so pressing twice sends once. A resend is a new row with a new key: SkyNotify sends it even when an earlier mail went out. A certificate issued before the queue has no row; `failed` does not take it, `all` does.

## What the panel reads

- `GET /v1/events/{id}/certificates/attendance`: an active certificate carries `mail` (its latest row): `status` (`queued`, `sent`, `failed`, `cancelled`), `attempts`, `errorCode`, `sentAt`, `updatedAt`. No `mail` for one issued before the queue.
- `GET /v1/events/{id}/certificates`: each certificate carries the same `mail`.
- `GET /v1/events/{id}/certificates/summary`: `mailQueuedCount` and `mailFailedCount`, over the active certificates.

## Without the queue

`certificate.NewService` (the synchronous embedders' contract) and any Mailer that is not a `certificate.CertificateMailer` send the mail inline as before; resend answers `400` there. On that synchronous path a mail the queue could not take is sent inline instead of being lost.

## Release

The release before this one reads and writes none of the table. Certificates issued before it have no row. Rolling back drops the table (the down migration): mails waiting in it are not sent.
