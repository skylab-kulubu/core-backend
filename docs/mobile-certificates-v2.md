# Mobile handoff — Certificates v2

Date: 2026-09-19

Mobile does not manage certificate templates or production jobs. It only lists the signed-in user's issued certificates and opens URLs supplied by core.

## Endpoint

`GET /v1/certificates/me`

- Requires the normal Bearer access token.
- Returns the list as the top-level JSON array; there is no `data` envelope.
- Core returns newest certificates first. Mobile may defensively sort by `issuedAt` descending.
- An empty array is a real empty state. Network and parsing failures must show an error with retry and must not become an empty list.

Example valid item:

```json
{
  "id": "66a7b7ff-f8c7-4a8d-96c9-f95cc83b43db",
  "serial": "75E614C7A33C08CB5C04804D6C24F9E7",
  "event": {
    "id": "77d8e777-3aa5-479b-ac8a-919de8c68e9e",
    "name": "AGC 2026",
    "ownerTeam": "AGC"
  },
  "status": "valid",
  "issuedAt": "2026-09-19T15:30:00Z",
  "pdfUrl": "https://api.yildizskylab.com/v1/certificates/verify/75E614C7A33C08CB5C04804D6C24F9E7/pdf",
  "verifyUrl": "https://skyl.app/c/75E614C7A33C08CB5C04804D6C24F9E7",
  "share": {
    "title": "AGC 2026 sertifikası",
    "text": "Ada Lovelace · AGC 2026",
    "url": "https://skyl.app/c/75E614C7A33C08CB5C04804D6C24F9E7"
  }
}
```

A revoked item has `status: "revoked"` and `revokedAt`. Its `pdfUrl` is omitted; `verifyUrl` and `share` remain available. Unknown future statuses must render as neutral/unavailable, never as valid.

## Screen behavior

- Row title: `event.name`.
- Subtitle: normalized issuer and localized issue date. Display `SKY LAB` when `event.ownerTeam` is empty, `YK`, or `DK`; otherwise display the OwnerTeam value.
- Show `Geçerli` or `İptal`. Revoked certificates stay visible as history.
- For a valid certificate, the primary action opens `pdfUrl` with the app's existing browser link flow.
- `Doğrula` opens `verifyUrl`.
- `Paylaş` uses `share.title`, `share.text`, and `share.url`. Share the verification URL, not the PDF URL.
- A revoked certificate can open the verification page but has no PDF action.

## Deliberately absent fields

Mobile receives no template, layout, Canva/Figma source, recipient email, Ticket id, batch/job data, role information, or internal storage key. It must not call admin certificate endpoints.

## Compatibility and errors

- Parse ISO-8601 timestamps as UTC-aware `DateTime` values and localize only at presentation time.
- Treat `revokedAt` and `pdfUrl` as nullable.
- Validate `pdfUrl`, `verifyUrl`, and `share.url` as HTTPS before opening them.
- A `401` follows the app's existing refresh/sign-out flow.
- A `403` is not an empty state; show the normal API error.
- Canonical share target: `https://skyl.app/c/{128-bit-opaque-serial}`.

## Acceptance checks

- No certificate: empty state.
- Valid certificate: download, verify, and share work.
- Revoked certificate: remains visible, has no PDF action, verification says revoked.
- Long Turkish Event names do not break the list.
- Offline/server failures show retry and never claim the user has no certificates.
- No template-definition or admin endpoint is called by the mobile screen.
