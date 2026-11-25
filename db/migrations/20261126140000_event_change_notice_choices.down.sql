SET LOCAL lock_timeout = '5s';

-- Rolling back drops the record of the editors' answers and the told
-- fields of the waiting notices (they then announce all of time and place,
-- as before). The release before neither writes nor reads them, so rolling
-- back the code alone needs no down migration.
DROP TABLE IF EXISTS event_change_notice_choices;
ALTER TABLE event_update_notifications DROP COLUMN IF EXISTS told_fields;
