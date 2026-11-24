-- Rolling back drops the waiting list's switch and the queued mail's kind:
-- a queued waitlisted or promoted row would then be sent as "Biletin hazır",
-- so those rows go first (a waitlisted Ticket stays waitlisted; the release
-- before keeps it off the door and the lists). The release before writes
-- neither column, so rolling back the code alone needs no down migration.
--
-- The audit action and basis checks stay wide, as the ticket status
-- migration's down leaves them: the rows already written are kept.
DELETE FROM ticket_notifications WHERE kind <> 'created';
DROP INDEX IF EXISTS tickets_waitlist_idx;
ALTER TABLE ticket_notifications DROP CONSTRAINT IF EXISTS ticket_notifications_kind_check;
ALTER TABLE ticket_notifications DROP COLUMN IF EXISTS kind;
ALTER TABLE events DROP COLUMN IF EXISTS waitlist_enabled;
