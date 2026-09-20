ALTER TABLE account_deletion_requests
    ADD COLUMN IF NOT EXISTS platform_blocked_at TIMESTAMPTZ;

ALTER TABLE account_deletion_outbox
    ALTER COLUMN available_at SET DEFAULT 'infinity'::timestamptz;

UPDATE account_deletion_outbox outbox
SET available_at = 'infinity'::timestamptz
FROM account_deletion_requests request
WHERE request.id = outbox.request_id
  AND request.platform_blocked_at IS NULL
  AND outbox.published_at IS NULL;

DROP INDEX IF EXISTS account_deletion_requests_claim_idx;
CREATE INDEX account_deletion_requests_claim_idx
    ON account_deletion_requests (next_attempt_at, created_at)
    WHERE platform_blocked_at IS NOT NULL
      AND status IN ('pending', 'processing');

DROP INDEX IF EXISTS account_deletion_requests_projection_idx;
CREATE INDEX account_deletion_requests_projection_idx
    ON account_deletion_requests (created_at, id)
    WHERE platform_blocked_at IS NULL;

DROP INDEX IF EXISTS account_deletion_outbox_pending_idx;
CREATE INDEX account_deletion_outbox_pending_idx
    ON account_deletion_outbox (available_at, created_at)
    WHERE published_at IS NULL;

COMMENT ON COLUMN account_deletion_requests.platform_blocked_at IS
    'Redis account-access marker confirmation; erasure and outbox work must not advance before this is non-null.';
