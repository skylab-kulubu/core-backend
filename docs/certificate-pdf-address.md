# Certificate PDF address

A certificate's PDF opens on the club's own address, as the QR's verification page already does: `https://yildizskylab.com/sertifika/{serial}.pdf` (Yusuf, 2026-11-25; Credly and Accredible keep their links on a lasting address of their own too). Before it, `pdfUrl` pointed at core's API host.

| | Address |
| --- | --- |
| QR, mail, `verifyUrl` | unchanged: `PUBLIC_VERIFY_ORIGIN/{serial}` (`https://skyl.app/c/{serial}`), which redirects to `https://yildizskylab.com/sertifika/{serial}` |
| `pdfUrl` with `CERTIFICATE_PUBLIC_PAGE_ORIGIN` set | `{CERTIFICATE_PUBLIC_PAGE_ORIGIN}/{serial}.pdf` |
| `pdfUrl` without it | `{PUBLIC_API_ORIGIN}/v1/certificates/verify/{serial}/pdf`, as before |
| core's own PDF route | `GET /v1/certificates/verify/{serial}/pdf` keeps answering, for the links already sent |

`pdfUrl` is in `GET /v1/public/certificates/{serial}`, `GET /v1/certificates/verify/{serial}` and `GET /v1/certificates/me`; a revoked certificate has none. The certificate mail carries no PDF link (`VerifyURL` only, [certificate-mail.md](certificate-mail.md)).

## The site

skylab-site serves `/sertifika/{serial}.pdf`: its server reads core's `GET /v1/certificates/verify/{serial}/pdf` and streams it on (`Content-Type: application/pdf`, `Content-Disposition: inline; filename="SKYLAB-sertifika-{serial}.pdf"`, `Cache-Control: no-store` unless core sends its own, `noindex`, no Referer). A serial that is not 16 or 32 hex characters is `404` without asking core; core's `404` (no such certificate, or revoked), `410` and `429` reach the visitor as they are; anything else is `502`. Core decides every time, so a revocation shows at once. The site asks core as the ticket page does: over `CORE_INTERNAL_URL` with the visitor's address as `X-Forwarded-For`, so the per-client limit on the public certificate routes counts each visitor. The verification page's "PDF" button links the same address.

## Configuration

`CERTIFICATE_PUBLIC_PAGE_ORIGIN`: the certificate page's address, no trailing slash needed.

| Environment | Value |
| --- | --- |
| production | `https://yildizskylab.com/sertifika` |
| sandbox | `https://sandbox.yildizskylab.com/sertifika` |

An address that is not absolute http(s) (or carries a query or fragment) stops core at startup. Unset, `pdfUrl` stays on core as before. The same variable sets where `/c/{serial}` and `/v1/go/c/{serial}` redirect; that redirect falls back to `https://yildizskylab.com/sertifika` when it is unset, so setting it in the sandbox also sends the sandbox's short links to the sandbox site.

**Order.** Release the site's `/sertifika/{serial}.pdf` first, then set the variable on core. Rollback: unset it (and restart core); `pdfUrl` goes back to core's own address, and the site's address keeps working.
