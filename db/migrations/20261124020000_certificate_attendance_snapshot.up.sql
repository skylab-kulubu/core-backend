-- The attendance a certificate was issued on (docs/certificate-rule.md):
-- the Ticket's check-ins on the live counted Sessions, their number N and the
-- program minutes of the counted Sessions it attended, all three as they were
-- at the issue. The public verification shows them. A certificate issued
-- before has none: the three stay NULL (they are not filled in afterwards).
ALTER TABLE certificates
    ADD COLUMN IF NOT EXISTS attended_count INTEGER,
    ADD COLUMN IF NOT EXISTS counted_total INTEGER,
    ADD COLUMN IF NOT EXISTS program_minutes INTEGER;
ALTER TABLE certificates DROP CONSTRAINT IF EXISTS certificates_attendance_snapshot_check;
ALTER TABLE certificates ADD CONSTRAINT certificates_attendance_snapshot_check CHECK (
    (attended_count IS NULL AND counted_total IS NULL AND program_minutes IS NULL)
    OR (attended_count IS NOT NULL AND counted_total IS NOT NULL AND program_minutes IS NOT NULL
        AND attended_count >= 0 AND counted_total >= 0 AND program_minutes >= 0)
);
