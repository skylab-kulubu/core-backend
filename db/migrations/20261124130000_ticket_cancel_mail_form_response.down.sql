-- Rolling back drops the forms answer of the Tickets and the cancelled kind.
-- The narrower kind check cannot be added while cancelled rows are queued,
-- so they go first: unsent cancellation mails are lost (the release before
-- would drop them anyway, its worker sends no mail of a Ticket that is not
-- active). A Ticket cancelled stays cancelled. The release before writes
-- neither, so rolling back the code alone needs no down migration.
DELETE FROM ticket_notifications WHERE kind = 'cancelled';
ALTER TABLE ticket_notifications DROP CONSTRAINT IF EXISTS ticket_notifications_kind_check;
ALTER TABLE ticket_notifications ADD CONSTRAINT ticket_notifications_kind_check
    CHECK (kind IN ('created', 'waitlisted', 'promoted'));
DROP INDEX IF EXISTS tickets_form_response_idx;
ALTER TABLE tickets DROP COLUMN IF EXISTS form_response_id;
