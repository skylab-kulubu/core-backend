# SkyPass in Google Wallet

A Member without sky-app adds their SkyPass to Google Wallet and shows it at the door. The pass face is the person's name and skyNumber, no photo (ADR-0024). Its QR code rotates on the phone every 60 seconds (Google's `rotatingBarcode` with TOTP, ADR-0023: "typically every minute", not the 3-second example of Google's how-to). The door scanner sends the code to core, which checks it and settles the check-in on the same route as the in-app SkyPass QR and the Student card.

Google only for now (Yusuf, 2026-10-07). Apple Wallet comes when the club has an Apple Developer account (platform-rebuild ticket 39).

Off until `SKYPASS_GOOGLE_WALLET_ENABLED=true` and its settings are in place ([Settings](#settings)). Off, the status route says `available: false`, the link routes answer `503 skypass_google_wallet_off`, and the door reads no Wallet code.

## For apps: getting the link

All three routes need the person's own token (any signed-in, active User, as for `POST /v1/skypass/qr`).

### `GET /v1/skypass/wallet`

Whether to show the "Add to Google Wallet" button.

```json
{ "google": { "available": true, "issued": false } }
```

- `available`: false while Google Wallet is off. Hide the button.
- `issued`: the person has a pass that opens the door. It does not say whether a phone saved it.

### `POST /v1/skypass/wallet/google`

Writes the person's pass to Google (a new pass the first time, otherwise the same pass with the current name and skyNumber) and answers its save link. No body.

```json
{ "saveUrl": "https://pay.google.com/gp/v/save/eyJhbGciOiJSUzI1NiIs…" }
```

Open `saveUrl` in the browser (on Android it hands over to Google Wallet). Ask for a new link each time the button is pressed; do not store, log or share it. The link names the pass only; the pass's secret never travels in it. The first Google account that saves the pass keeps it: a forwarded link cannot put the pass on someone else's account (the class is `ONE_USER_ALL_DEVICES`). Google Wallet asks the phone to be unlocked each time the pass is opened.

| Status | `code` | Meaning |
| --- | --- | --- |
| 200 | | `saveUrl` |
| 401 | | no token |
| 404 | | the person is not active (deletion pending or erased) |
| 409 | | the pass was ended (a revoke, or the account's deletion) while the link was written; no link. Ask again |
| 429 | `skypass_wallet_link_rate_limited` | more than 10 link or revoke calls in a minute; `retryAfterSeconds` |
| 502 | `skypass_google_wallet_unavailable` | Google did not take the write; `Retry-After: 30` |
| 503 | `skypass_google_wallet_off` | Google Wallet is off |

### `DELETE /v1/skypass/wallet/google`

Ends the person's pass, for a lost or replaced phone: its codes stop opening the door at once, and Google's copy goes inactive without the name or skyNumber. The next `POST` makes a new pass with a new secret. `204`, also when there was none. If Google is down the answer is `502 skypass_google_wallet_unavailable`, but the codes have already stopped; the next `POST` (or erasure) withdraws Google's copy.

## For the door scanner

A Wallet code is the QR value `SPW1:<pass id>:<8 digits>`, for example `SPW1:MKEJHZR37KOZLLPVDAOC4E62HE:04718392`. It contains only QR alphanumeric characters, so the code stays small. The scanner tells it apart from an in-app token (a JWT, `eyJ…`) by the `SPW1:` prefix.

**Online only.** The code is a TOTP of a secret only core and Google hold, so the scanner cannot check it on the device (unlike the in-app token, ADR-0022) and does not queue it while core is unreachable. Without core, the person shows their Student card or the desk finds them by name.

| Route | What it does with a Wallet code |
| --- | --- |
| `POST /v1/sessions/{sessionId}/check-in/skypass` `{"token": "SPW1:…"}` | Checks the holder in, as for an in-app token. It **spends** the code: the same code, or an older one of the same pass, checks no one in again. |
| `POST /v1/skypass/verify` `{"token": "SPW1:…"}` | Answers the holder (`id`, `skyNumber`, `firstName`, `lastName`) without spending the code. A spent code is refused here too. Same permission as for in-app tokens (`ticket:validate`). |

Answers specific to Wallet codes (others as for in-app tokens):

| Status | `code` | Meaning | Scanner shows |
| --- | --- | --- | --- |
| 400 | | not a code of a live pass, wrong digits, or Google Wallet is off | invalid |
| 404 | | the holder is not active | invalid |
| 409 | `skypass_wallet_code_used` | this code already checked someone in (a second door, a screenshot, a retry of a request that went through) | "used; the pass shows a new code within a minute" |
| 429 | `skypass_wallet_rate_limited` | ten wrong codes for this pass from this scanner in ten minutes; `Retry-After`, `retryAfterSeconds` | wait |

A 409 `skypass_wallet_code_used` after a network retry usually means the first request went through: the session's door activity (`GET /v1/sessions/{id}/check-ins`) shows it. A duplicate check-in of the same Oturum is still the ticket route's `409` (no code).

## The pass

- **Class:** one GenericClass per environment, `<issuer>.<SKYPASS_GOOGLE_WALLET_CLASS_SUFFIX>` (`…skypass-production`, `…skypass-sandbox`), so sandbox passes never share a class with production ones. Core writes it once per process (insert, or patch when it exists): `multipleDevicesAndHoldersAllowedStatus: ONE_USER_ALL_DEVICES`, `viewUnlockRequirement: UNLOCK_REQUIRED_TO_VIEW`.
- **Object:** one GenericObject per pass, `<issuer>.sp-<pass id>`, inserted over the REST API with the issuer's service account (`wallet_object.issuer` scope); on later links it is replaced (PUT) with the current name and skyNumber. The save link is a signed `savetowallet` JWT (RS256, the service account's key) that names the object by id and class only. Google's rotating barcode guidance asks for exactly this, so the TOTP key never sits in a link.
- **Face:** card title `SKY LAB`, subheader `SkyPass`, header the person's name, the skyNumber under the barcode (`alternateText`) and in a text module. No photo, no e-mail. `GENERIC_OTHER`, `passConstraints.screenshotEligibility: INELIGIBLE` (Android blocks screenshots; older Wallet versions may still allow them). Logo only if `SKYPASS_GOOGLE_WALLET_LOGO_URL` is set.
- **Barcode:** `rotatingBarcode` `QR_CODE`, `valuePattern` `SPW1:<pass id>:{totp_value_0}`, `totpDetails` `TOTP_SHA1`, `periodMillis` `60000`, one parameter with the pass's key (Base16) and `valueLength` 8.
- **Withdrawn pass:** Google's API has no delete for objects, so core replaces the object with an inactive one (`state: INACTIVE`, header `SkyPass`, subheader `Geçersiz`, no barcode, no name, no skyNumber). It moves to the phone's expired passes.

`skypass_google_wallet_passes` keeps one row per pass: `pass_id`, `user_id`, `last_counter` (the highest TOTP step a check-in took), `created_at`, `revoked_at`. No secret, no name. At most one pass per person is not revoked. A revoked row stays only until Google's copy is withdrawn, then it is deleted.

## Codes

- **Pass id:** 16 random bytes, Base32 without padding (26 characters). It is public: every barcode shows it.
- **Secret:** HKDF-SHA256 of `SKYPASS_GOOGLE_WALLET_TOTP_KEY`, no salt, info `skylab skypass google wallet totp v1\x00` + pass id, 20 bytes (RFC 4226's recommended 160 bits). Nothing secret is stored per pass. Every replica derives the same secret; a new pass id is a new secret.
- **TOTP:** RFC 6238 with HMAC-SHA-1 (the only algorithm Google offers), T0 the Unix epoch, 60-second steps, 8 digits.
- **Window:** the step of core's clock, one before and one after (a phone clock up to a minute off, or a scan that took a while).
- **One use:** a check-in spends the code's step with one conditional update (`last_counter < step`), so two scans of the same code at once check in at most one. A code of a spent step or an older one is refused (`skypass_wallet_code_used`), also on `verify`. The pass's next code works.
- **Who:** the pass's holder must be an active User, as for in-app tokens. Whether the scanner may check anyone in at that Event is the check-in route's decision, unchanged.
- **Wrong codes:** ten per scanner and pass in ten minutes, then that scanner's codes for that pass wait for the window's end. Keyed by scanner too, so a stranger who saw the pass id cannot lock its holder out. A guess is about one in 33 million (three valid codes of 8 digits), and a right guess only checks the pass's holder in, which door staff can already do by name.

## Revocation and erasure

- **Lost phone, new phone:** the person revokes (`DELETE /v1/skypass/wallet/google`) and adds the pass again.
- **Name or skyNumber changed:** the next link rewrites the pass; a saved pass keeps the old face until then.
- **Deletion pending:** codes are refused from the moment the person asked (the holder is no longer active), and no new pass can be made for them (the table's account reference guard).
- **A link written while the pass ends:** a revoke or an erasure can withdraw the pass while a link request is writing it to Google, and Google may take the link's write last. The link request reads the pass again after its write; if it has ended, it withdraws it once more and answers `409` without a link.
- **Erasure:** the `erase_skypass_wallet` step ([account-lifecycle.md](account-lifecycle.md#durable-erasure-flow)), right after `erase_contact_consents` and before the services, withdraws every pass of the person and deletes the rows. Google down or busy (no answer, 429, 5xx) defers the step like a service that is down; a refusal (for example a key Google no longer takes) spends attempts and ends in manual intervention. With Google Wallet switched off while a pass is left, the step revokes it (its codes stop) and fails, so the request goes to manual intervention rather than being reported erased while Google still holds the name: turn Google Wallet back on, or withdraw the object by hand, then retry.
- **Leaving the club:** nothing is withdrawn. As with the in-app SkyPass, the door decides by the Ticket of the Event, not by the card.

## Settings

| Variable | Required with `true` | Value |
| --- | --- | --- |
| `SKYPASS_GOOGLE_WALLET_ENABLED` | | `true` or `false` (unset: `false`). `false` reads none of the others. |
| `SKYPASS_GOOGLE_WALLET_ISSUER_ID` | yes | the issuer's number (Google Pay & Wallet console) |
| `SKYPASS_GOOGLE_WALLET_CLASS_SUFFIX` | yes | this side's class, for example `skypass-production`, `skypass-sandbox` (letters, digits, `.`, `_`, `-`) |
| `SKYPASS_GOOGLE_WALLET_SERVICE_ACCOUNT_JSON` | yes | the service account's JSON key, base64-encoded on one line (raw JSON also works). An OpenBao reference in Dokploy (ADR-0049). |
| `SKYPASS_GOOGLE_WALLET_TOTP_KEY` | yes | 32 random bytes, unpadded base64url. An OpenBao reference in Dokploy. Each side its own. |
| `SKYPASS_GOOGLE_WALLET_ORIGINS` | no | comma-separated web origins allowed to show Google's save button for core's links (the JWT's `origins`); `https`, or `http` on localhost |
| `SKYPASS_GOOGLE_WALLET_LOGO_URL` | no | an `https` logo for the pass |

With `true`, a missing or malformed setting stops core at startup; the error names the variable, never the value. Startup logs `skypass google wallet: on (class …, service account …)` or `… off`.

**Do not rotate `SKYPASS_GOOGLE_WALLET_TOTP_KEY` while passes are out** unless it leaked: every saved pass stops opening the door until its holder asks for the link again (`POST /v1/skypass/wallet/google` rewrites the pass with the key of the moment, and Google updates the saved pass on the phone). There is no key version in v1.

## Metrics

On `/v1/metrics` (internal only), without any person, pass or code:

- `skylab_skypass_google_wallet_enabled` (gauge)
- `skylab_skypass_wallet_codes_total{outcome}`: `accepted`, `malformed`, `unknown_pass`, `revoked`, `wrong_code`, `used`, `inactive_account`, `rate_limited`, `off`
- `skylab_skypass_wallet_code_skew_total{step}`: `-1`, `0`, `1` for accepted codes; many `-1`/`1` is a clock that is off
- `skylab_skypass_wallet_links_total{outcome}`: `issued`, `failed`, `off`
- `skylab_skypass_wallet_withdrawals_total{reason,outcome}`: reasons `member`, `erasure`, `retry`

Logs name the operation, the HTTP status and Google's error status (`INVALID_ARGUMENT`, `PERMISSION_DENIED`, …), never Google's message (it may quote what was sent), a token, a key, a code or a link.

## Against the standards

What matches:

- Google's rotating barcode guidance: the object is inserted over the API and the save JWT references it by id, so the key is not in the JWT; one OTP key per pass; the reader accepts the newest code only once.
- RFC 6238: SHA-1 TOTP from the Unix epoch, a one-step window, and a code is not accepted twice after a successful validation (§5.2). RFC 4226 §4: 160-bit secrets; §7: a throttle on wrong values.
- ADR-0023: 60-second period, optional screenshot block used. ADR-0024: name and skyNumber, no photo.

Deviations:

- **Online only, no Queued check-in (ADR-0022).** ADR-0022 and CONTEXT ("a 60-second Google code … valid at the door if unexpired and signed") assume the scanner checks a Wallet code on the device. A TOTP is not a signature, and sending every pass's secret to staff phones is not acceptable, so the door is fail-closed for Wallet codes when core is down; the Student card and the desk remain. An e-skylab ADR amendment should record this.
- **One step forward too.** RFC 6238 recommends at most one step backward; core also takes one step ahead, for a phone clock that runs fast.
- **`verify` does not spend.** A staff check of who the code belongs to leaves the code for the check-in; only the check-in spends it.
- **A spent code is spent before the check-in is written.** If the check-in then fails (no Ticket, the scanner may not check in at that Event), the code stays spent and the pass's next code (within a minute) is needed.
- **Derived, not stored, secrets.** The common pattern stores a random secret per pass, encrypted at rest (core's OpenBao Transit). Here the secret is derived from one key and the public pass id: nothing secret at rest and the door does not depend on OpenBao. The cost: whoever holds the key and has seen a barcode can compute that pass's codes, the same trust the in-app SkyPass signing key carries, and after a key rotation every holder has to ask for the link again.
- **Unlock to view.** Transit passes usually open without unlocking; a membership credential asks for the unlock, so a lost locked phone does not show it.
- **No Smart Tap (NFC).** It needs a certified terminal; it is the later slice (CONTEXT: Wallet).
- **Withdrawal, not deletion.** Google has no delete for objects; core overwrites the object without the person's data and marks it inactive. Whatever history Google keeps of earlier versions is Google's (Google Wallet API terms).
