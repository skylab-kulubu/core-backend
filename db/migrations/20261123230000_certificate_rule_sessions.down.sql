-- Rolls back the certificate rule's threshold and Session marks. An Event
-- on the count rule goes back to the ratio rule at 0.75, the release
-- before's nearest rule (its default threshold is ceil(3N/4)).
UPDATE events SET attendance_rule = 'ratio', attendance_ratio = 0.75 WHERE attendance_rule = 'count';

ALTER TABLE sessions
    DROP CONSTRAINT IF EXISTS sessions_required_is_counted,
    DROP COLUMN IF EXISTS certificate_required,
    DROP COLUMN IF EXISTS certificate_counted;

ALTER TABLE events
    DROP CONSTRAINT IF EXISTS events_attendance_min_count_check,
    DROP COLUMN IF EXISTS attendance_min_count;
