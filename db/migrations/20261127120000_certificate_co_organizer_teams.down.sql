SET LOCAL lock_timeout = '5s';

-- Rolls back nothing: the column stays, so the snapshots already written
-- (what a verification page shows for a certificate issued since) are not
-- lost. The release before neither writes nor reads it, and the up
-- migration finds it again (IF NOT EXISTS).
SELECT 1;
