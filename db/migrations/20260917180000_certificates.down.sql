DROP INDEX IF EXISTS certificates_event_id_idx;
DROP INDEX IF EXISTS certificates_owner_id_idx;
DROP INDEX IF EXISTS certificates_event_ticket_active_idx;
DROP TABLE IF EXISTS certificates;

ALTER TABLE sessions DROP COLUMN IF EXISTS cancelled;

ALTER TABLE events
    DROP COLUMN IF EXISTS attendance_ratio,
    DROP COLUMN IF EXISTS attendance_rule;
