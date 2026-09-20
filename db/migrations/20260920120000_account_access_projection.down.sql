LOCK TABLE account_deletion_requests, account_deletion_outbox IN ACCESS EXCLUSIVE MODE;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM account_deletion_requests) THEN
        RAISE EXCEPTION 'cannot remove account access projection while a durable deletion request exists';
    END IF;
END
$$;

ALTER TABLE account_deletion_outbox
    ALTER COLUMN available_at SET DEFAULT now();

UPDATE account_deletion_outbox
SET available_at = created_at
WHERE available_at = 'infinity'::timestamptz
  AND published_at IS NULL;

DROP INDEX IF EXISTS account_deletion_requests_projection_idx;
DROP INDEX IF EXISTS account_deletion_requests_claim_idx;
CREATE INDEX account_deletion_requests_claim_idx
    ON account_deletion_requests (next_attempt_at, created_at)
    WHERE status IN ('pending', 'processing');

ALTER TABLE account_deletion_requests DROP COLUMN platform_blocked_at;
