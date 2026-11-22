-- Rolls back the Ticket's access key, code and QR version. The ticket
-- pages and Ticket QRs sent out stop working; the release before this one
-- reads none of the three columns.
SET LOCAL lock_timeout = '5s';

DROP INDEX IF EXISTS tickets_code_idx;
DROP INDEX IF EXISTS tickets_access_key_idx;
ALTER TABLE tickets
    DROP COLUMN IF EXISTS qr_version,
    DROP COLUMN IF EXISTS code,
    DROP COLUMN IF EXISTS access_key;
DROP FUNCTION IF EXISTS ticket_random_text(TEXT, INTEGER);
