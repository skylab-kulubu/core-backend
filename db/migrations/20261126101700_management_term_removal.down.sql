SET LOCAL lock_timeout = '5s';

-- Rolling back drops the runs' removal counts. The audit basis check stays
-- wide, as the earlier audit migrations' downs leave it: the member.remove
-- rows the removal already wrote are kept. The release before writes no
-- such row and no removal count, so rolling back the code alone needs no
-- down migration.
ALTER TABLE management_term_runs DROP CONSTRAINT IF EXISTS management_term_runs_removal_check;
ALTER TABLE management_term_runs DROP COLUMN IF EXISTS removed;
ALTER TABLE management_term_runs DROP COLUMN IF EXISTS removal;
