-- An Event's place on the map (bilet PDF v5; docs/event-location.md): the
-- point its organizers pick in the panel, and the street address they may
-- add. location stays the place's name as people say it ("YTÜ Davutpaşa,
-- EEF Konferans Salonu"); the point is optional and comes as a pair: both
-- or neither. The ticket's "Etkinlik yeri" and its directions link read
-- them.
--
-- The point is in WGS 84 degrees, as a map gives it; core keeps six
-- decimals (about 11 cm), which is finer than any building. The address is
-- free text of at most 300 characters.
--
-- No personal data: an Event's place is public. The release before this one
-- keeps working on the new schema: its inserts take the defaults (no point,
-- no address) and its updates leave the three columns as they are.
SET LOCAL lock_timeout = '5s';

ALTER TABLE events
    ADD COLUMN IF NOT EXISTS location_lat DOUBLE PRECISION,
    ADD COLUMN IF NOT EXISTS location_lng DOUBLE PRECISION,
    ADD COLUMN IF NOT EXISTS location_address TEXT NOT NULL DEFAULT '';

ALTER TABLE events
    DROP CONSTRAINT IF EXISTS events_location_point_check,
    ADD CONSTRAINT events_location_point_check CHECK (
        (location_lat IS NULL) = (location_lng IS NULL)
        AND (location_lat IS NULL OR (
            location_lat BETWEEN -90 AND 90
            AND location_lng BETWEEN -180 AND 180
            AND NOT (location_lat = 0 AND location_lng = 0)
        ))
        AND char_length(location_address) <= 300
    );
