# SkyMail templates

Core sends two mails through SkyMail: the welcome mail a new account receives
and the certificate mail that follows an issued certificate. Both are posted to
`/v1/mail_tasks/single` and neither is awaited. A mail must not fail the request
that produced it, so the caller is told nothing.

That is also how these mails used to disappear. SkyMail refuses a send whose
template is archived or unknown, and writes no `mail_tasks` row at all. Core
threw the response away without reading its status, so a wrong or archived
template left no mail, no row, no error and no log line — the send looked
identical to a delivered one from inside core.

## Addressing a template

A send carries exactly one addressing:

- `template_key` — a stable name SkyMail resolves at send time. `core.welcome`
  and `core.certificate` are system templates there: they cannot be archived and
  their key cannot be renamed, so a template can be replaced underneath core
  without core being redeployed.
- `template_id` — the template's UUID. It stops matching the moment the row is
  archived or replaced, which is the failure this page exists for.

The key wins whenever it is set, and the id is then not sent at all. SkyMail
stops looking at the key as soon as an id is present, so a body holding both
would silently pin the mail to the id and hide which addressing the deployment
believed it was using.

One exception runs in the other direction. If the key is set and SkyMail answers
`404` — the key is not seeded yet — core retries that send once with the
template id, if one is configured, and says so in the log. There is no reverse
fallback: a refused id is never retried with a key.

The welcome template reads `{{.FirstName}}`. Core sends `FirstName`, `LastName`,
`Email`, `SkyNumber` and `CreatedAt`, which covers both templates.

## Configuration

| Variable | Unset | Empty |
| --- | --- | --- |
| `SKYMAIL_WELCOME_TEMPLATE_KEY` | `core.welcome` | welcome mail falls back to its id |
| `SKYMAIL_CERTIFICATE_TEMPLATE_KEY` | `core.certificate` | certificate mail falls back to its id |
| `SKYMAIL_WELCOME_TEMPLATE_ID` | no id addressing | no id addressing |
| `SKYMAIL_CERTIFICATE_TEMPLATE_ID` | no id addressing | no id addressing |

An unset key variable takes the seeded default, because addressing by key is
what core wants everywhere. A variable set to an empty value is the way back
onto the template id without a code change; in compose that difference is why
the keys use `${VAR-default}` rather than `${VAR:-default}`.

A mail kind with neither a key nor an id is never sent, exactly as before. The
difference is that startup now says so once.

## What a refused call writes

Every SkyMail call — both sends, the mailing list calls and the token request —
checks the response status. Anything outside `2xx` writes one line:

```
{"event":"skymail_call_failed","level":"warn","kind":"welcome","status":404,"reason":"template_missing","code":"template_not_found"}
```

- `kind` — `welcome`, `certificate`, `token`, or the list call (`list_create`,
  `list_delete`, `list_read`, `list_recipients`, `list_recipient_add`,
  `list_recipient_remove`).
- `status` — the raw status SkyMail answered with. Nothing is inferred from it
  beyond the reason below, because an archived template has not been observed
  often enough to promise it is always a `404`.
- `reason` — `template_missing` for a `404` to a send,
  `template_key_missing_fallback_to_id` for the line that precedes the one
  retry, and `upstream_error` for everything else.
- `code` — the `error` or `message` field of a small JSON body, and only when it
  is a bare code: short, and made of letters, digits, `_`, `-` and `.`. Prose,
  mail addresses and identifiers are dropped. The line carries no recipient, no
  template, no token and no body; at most 2 KB of a failed response is read at
  all, and a longer one is truncated before it is parsed.

A mailing list call that answers `404` writes nothing: callers already receive
`ErrListNotFound` and act on it.

Startup writes one line per mail kind that cannot be addressed at all:

```
{"event":"skymail_template_unconfigured","level":"warn","kind":"certificate","reason":"template_unconfigured"}
```

The send path stays silent for that case, so a half-configured deployment is
visible once at boot instead of once per user.

## Moving a template onto its key

The keys are not in the live SkyMail database yet; what is there are the older
rows addressed by UUID. The order matters, because the half-configured state in
between is exactly what used to lose mail silently.

1. Seed the template in SkyMail: `PUT /v1/templates/by-key/:key` for
   `core.welcome` and `core.certificate`.
2. Deploy core with the key variables (or with them unset, which is the same
   thing). Until step 1 has run for a key, core logs
   `template_key_missing_fallback_to_id` and the mail still goes out through the
   template id.
3. Archive the old UUID-addressed rows and remove `SKYMAIL_WELCOME_TEMPLATE_ID`
   and `SKYMAIL_CERTIFICATE_TEMPLATE_ID`.

Dropping the id path from core is a separate change, made once step 3 has held
in production, and it is worth making: a deployment that keeps both addressings
configured is the state this page keeps warning about.
