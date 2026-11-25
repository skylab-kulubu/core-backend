SET LOCAL lock_timeout = '5s';

-- Rolling back drops every Co-organizer: the release before reads none, and
-- the Owner team keeps the Event.
DROP TABLE IF EXISTS event_co_organizers;
