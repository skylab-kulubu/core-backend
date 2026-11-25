SET LOCAL lock_timeout = '5s';

-- Rolling back drops the digests: a waiting row that told details alone
-- then tells nothing (its time and place did not change). The release
-- before neither writes nor reads them.
ALTER TABLE event_update_notifications DROP COLUMN IF EXISTS before_details;
