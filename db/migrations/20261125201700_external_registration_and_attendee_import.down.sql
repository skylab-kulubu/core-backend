-- Rolling back drops the external registration link. The audit action and
-- basis checks stay wide, as the earlier ticket migrations' downs leave
-- them: the ticket.import rows already written are kept.
SET LOCAL lock_timeout = '5s';

ALTER TABLE events DROP CONSTRAINT IF EXISTS events_external_registration_url_check;
ALTER TABLE events DROP COLUMN IF EXISTS external_registration_url;
