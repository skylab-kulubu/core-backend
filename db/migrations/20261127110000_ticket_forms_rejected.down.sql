SET LOCAL lock_timeout = '5s';

-- Rolling back keeps tickets.forms_rejected_at, form_response_form_id and
-- form_response_status with their values: the release before neither reads
-- nor writes them (it cancels a declined answer's Ticket again), and the
-- next release reads them again (the up migration finds them, ADD COLUMN IF
-- NOT EXISTS). To remove them for good, by hand: ALTER TABLE tickets DROP
-- COLUMN forms_rejected_at, DROP COLUMN form_response_form_id, DROP COLUMN
-- form_response_status;
SELECT 1;
