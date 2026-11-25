SET LOCAL lock_timeout = '5s';

-- Rolling back keeps tickets.forms_rejected_at and its marks: the release
-- before neither reads nor writes it (it cancels a declined answer's Ticket
-- again), and the next release reads the marks again (the up migration
-- finds the column, ADD COLUMN IF NOT EXISTS). To remove it for good, by
-- hand: ALTER TABLE tickets DROP COLUMN forms_rejected_at;
SELECT 1;
