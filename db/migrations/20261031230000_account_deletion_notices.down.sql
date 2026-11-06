-- Without the columns a request whose notice went out would send it again.
-- The rows keep no address, so nothing else is lost.
SET LOCAL lock_timeout = '5s';

ALTER TABLE account_deletion_requests
    DROP COLUMN IF EXISTS completed_notice_at,
    DROP COLUMN IF EXISTS received_notice_at;
