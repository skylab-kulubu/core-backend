-- Rolls back an Event's point and address; the Events keep their place's
-- name (location). The release before this one reads neither column.
SET LOCAL lock_timeout = '5s';

ALTER TABLE events
    DROP CONSTRAINT IF EXISTS events_location_point_check,
    DROP COLUMN IF EXISTS location_address,
    DROP COLUMN IF EXISTS location_lng,
    DROP COLUMN IF EXISTS location_lat;
