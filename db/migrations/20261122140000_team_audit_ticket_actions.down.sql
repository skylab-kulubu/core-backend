-- Nothing to roll back: the release before writes no ticket action, and the
-- wider check refuses nothing it writes. The ticket rows of the record stay
-- (the audit record is kept three years, retention rule team_audit_age).
SELECT 1;
