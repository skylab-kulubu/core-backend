SET LOCAL lock_timeout = '5s';

-- Rolling back keeps events.theme and events.ticket_accent_color with
-- their values and checks: the release before neither reads nor writes
-- them (every Event is Kurumsal to it), and the next release reads them
-- again (the up migration finds them, ADD COLUMN IF NOT EXISTS). To remove
-- them for good, by hand: ALTER TABLE events DROP COLUMN theme, DROP
-- COLUMN ticket_accent_color;
SELECT 1;
