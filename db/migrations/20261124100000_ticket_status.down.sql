-- Rolling back drops the Tickets' status: a cancelled (or waitlisted)
-- Ticket becomes an ordinary Ticket again, as every Ticket was before. The
-- release before writes no status, so rolling back the code alone needs no
-- down migration: leave the columns (the old code reads every Ticket, a
-- cancelled one included, until the new release is back).
--
-- The audit action check stays wide: the release before writes neither
-- ticket.cancel nor ticket.restore, and the rows already written are kept
-- (team_audit_age).
DROP INDEX IF EXISTS tickets_event_status_idx;
ALTER TABLE tickets DROP CONSTRAINT IF EXISTS tickets_cancelled_check;
ALTER TABLE tickets DROP CONSTRAINT IF EXISTS tickets_cancel_reason_check;
ALTER TABLE tickets DROP CONSTRAINT IF EXISTS tickets_cancelled_by_check;
ALTER TABLE tickets DROP CONSTRAINT IF EXISTS tickets_status_check;
ALTER TABLE tickets DROP COLUMN IF EXISTS cancel_reason;
ALTER TABLE tickets DROP COLUMN IF EXISTS cancelled_by;
ALTER TABLE tickets DROP COLUMN IF EXISTS cancelled_at;
ALTER TABLE tickets DROP COLUMN IF EXISTS waitlisted_at;
ALTER TABLE tickets DROP COLUMN IF EXISTS status_changed_at;
ALTER TABLE tickets DROP COLUMN IF EXISTS status;
