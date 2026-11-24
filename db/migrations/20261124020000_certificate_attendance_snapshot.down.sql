-- Drops the certificates' attendance snapshots: the verification page shows
-- no attendance line again, and the snapshots taken since are lost.
ALTER TABLE certificates
    DROP CONSTRAINT IF EXISTS certificates_attendance_snapshot_check,
    DROP COLUMN IF EXISTS attended_count,
    DROP COLUMN IF EXISTS counted_total,
    DROP COLUMN IF EXISTS program_minutes;
