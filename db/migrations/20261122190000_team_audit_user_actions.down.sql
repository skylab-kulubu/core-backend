-- Nothing to roll back: the release before writes no user.* action, and the
-- wider checks refuse nothing it writes. The user rows of the record stay
-- (the audit record is kept three years, retention rule team_audit_age).
DROP INDEX IF EXISTS team_audit_records_user_idx;
