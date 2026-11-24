-- The certificate rule "at least M of the N counted Sessions"
-- (docs/certificate-rule.md): the Event's threshold M (NULL is the default,
-- ceil(3N/4)) and each Session's two marks, counted (it is one of the N)
-- and required (missing it refuses the certificate). A required Session is
-- always counted. Existing Sessions are counted and not required and
-- existing Events keep no threshold, so no Event's eligibility changes.
-- attendance_rule has no check in the database: the application accepts
-- 'count' (and keeps an existing 'ratio').
ALTER TABLE events ADD COLUMN IF NOT EXISTS attendance_min_count INTEGER;
ALTER TABLE events DROP CONSTRAINT IF EXISTS events_attendance_min_count_check;
ALTER TABLE events ADD CONSTRAINT events_attendance_min_count_check
    CHECK (attendance_min_count IS NULL OR attendance_min_count >= 1);

ALTER TABLE sessions
    ADD COLUMN IF NOT EXISTS certificate_counted BOOLEAN NOT NULL DEFAULT true,
    ADD COLUMN IF NOT EXISTS certificate_required BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE sessions DROP CONSTRAINT IF EXISTS sessions_required_is_counted;
ALTER TABLE sessions ADD CONSTRAINT sessions_required_is_counted
    CHECK (NOT certificate_required OR certificate_counted);
