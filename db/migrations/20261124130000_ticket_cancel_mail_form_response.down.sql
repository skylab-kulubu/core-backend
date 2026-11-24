-- Rolling back drops the forms answer of the Tickets and the cancelled kind:
-- a queued cancelled row would be sent as "Biletin hazır" by the release
-- before, so those rows go first. A Ticket cancelled stays cancelled. The
-- release before writes neither, so rolling back the code alone needs no
-- down migration (turn TICKET_STATUS_MAIL off and let the queue drain
-- first).
DELETE FROM ticket_notifications WHERE kind = 'cancelled';
ALTER TABLE ticket_notifications DROP CONSTRAINT IF EXISTS ticket_notifications_kind_check;
ALTER TABLE ticket_notifications ADD CONSTRAINT ticket_notifications_kind_check
    CHECK (kind IN ('created', 'waitlisted', 'promoted'));
DROP INDEX IF EXISTS tickets_form_response_idx;
ALTER TABLE tickets DROP COLUMN IF EXISTS form_response_id;
