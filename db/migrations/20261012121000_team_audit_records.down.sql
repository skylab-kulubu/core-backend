-- Rolling back drops the audit record with its rows.
DROP TABLE IF EXISTS team_audit_records;
DROP FUNCTION IF EXISTS team_audit_records_finish_only();
