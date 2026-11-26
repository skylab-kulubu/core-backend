SET LOCAL lock_timeout = '5s';

-- Rolling back keeps event_form_mappings and its rows: the release before
-- neither reads nor writes the table, and dropping it would lose the
-- organizers' mappings, which the next release reads again (the up
-- migration finds the table, CREATE TABLE IF NOT EXISTS). To remove it for
-- good, by hand: DROP TABLE event_form_mappings;
SELECT 1;
