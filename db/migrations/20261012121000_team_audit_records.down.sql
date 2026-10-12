-- Do not run this in production: it destroys the audit record, which is
-- kept three years. To roll the release back, roll back the code only and
-- leave the table; the up migration (CREATE ... IF NOT EXISTS, with its
-- fingerprint) is safe to apply again over it.
-- Rolling back drops the audit record with its rows.
DROP TABLE IF EXISTS team_audit_records;
DROP FUNCTION IF EXISTS team_audit_records_finish_only();
