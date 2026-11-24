-- The certificate mail's queue (docs/certificate-mail.md; A7). Issuing a
-- certificate writes one 'issued' row; "Sertifikaları yeniden gönder" in
-- the panel writes 'resend' rows. A worker sends each row to SkyMail under
-- the Idempotency-Key core-certificate-mail:<id>, tries again with backoff
-- while SkyMail is unreachable, and keeps the outcome: sent, failed (given
-- up, or refused) or cancelled (the certificate was revoked meanwhile).
--
-- A row holds ids, never an address or a name: those are read from the
-- certificate when it is sent, and the row goes with its certificate.
--
--   * certificate_mails_issued_key: one 'issued' row per certificate, so a
--     job that issues again after a crash never queues a second mail.
--   * certificate_mails_pending_key: at most one queued row per
--     certificate, so pressing "yeniden gönder" twice sends once.
--
-- The release before this one keeps working on the new schema: it reads and
-- writes none of it.
SET LOCAL lock_timeout = '5s';

CREATE TABLE IF NOT EXISTS certificate_mails (
    id UUID PRIMARY KEY,
    certificate_id UUID NOT NULL REFERENCES certificates (id) ON DELETE CASCADE,
    event_id UUID NOT NULL REFERENCES events (id) ON DELETE CASCADE,
    reason TEXT NOT NULL CHECK (reason IN ('issued', 'resend')),
    status TEXT NOT NULL DEFAULT 'queued' CHECK (status IN ('queued', 'sent', 'failed', 'cancelled')),
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    claimed_until TIMESTAMPTZ,
    claim_token UUID,
    error_code TEXT NOT NULL DEFAULT '' CHECK (length(error_code) <= 64),
    requested_by UUID REFERENCES users (id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    sent_at TIMESTAMPTZ,
    CONSTRAINT certificate_mails_sent_check CHECK ((status = 'sent') = (sent_at IS NOT NULL))
);

CREATE UNIQUE INDEX IF NOT EXISTS certificate_mails_issued_key
    ON certificate_mails (certificate_id) WHERE reason = 'issued';
CREATE UNIQUE INDEX IF NOT EXISTS certificate_mails_pending_key
    ON certificate_mails (certificate_id) WHERE status = 'queued';
CREATE INDEX IF NOT EXISTS certificate_mails_due_idx
    ON certificate_mails (next_attempt_at) WHERE status = 'queued';
CREATE INDEX IF NOT EXISTS certificate_mails_event_idx
    ON certificate_mails (event_id, certificate_id, created_at DESC);
