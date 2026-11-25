SET LOCAL lock_timeout = '5s';

-- Rolling back keeps every row: event_form_respondents, respondent_ticket_jobs,
-- and events.form_responses_auto_ticket stay
-- (the release before neither reads nor writes them, and the next release
-- reads them again: the up migration finds them, IF NOT EXISTS).
--
-- form_pulls takes back its checks without the collect kind, NOT VALID so
-- that a collect job waiting now stays. The release before would run such a
-- job as a backfill (writing Tickets for every accepted answer), so it is
-- parked: next_attempt_at 'infinity' is never due; the up migration puts it
-- back. To remove the new objects for good, by hand:
--   DROP TABLE respondent_ticket_jobs; DROP TABLE event_form_respondents;
--   ALTER TABLE events DROP COLUMN form_responses_auto_ticket;
--   DELETE FROM form_pulls WHERE kind = 'collect';
UPDATE form_pulls SET next_attempt_at = 'infinity' WHERE kind = 'collect';

ALTER TABLE form_pulls DROP CONSTRAINT IF EXISTS form_pulls_kind_check;
ALTER TABLE form_pulls ADD CONSTRAINT form_pulls_kind_check CHECK (kind IN ('backfill', 'catchup')) NOT VALID;
ALTER TABLE form_pulls DROP CONSTRAINT IF EXISTS form_pulls_shape_check;
ALTER TABLE form_pulls ADD CONSTRAINT form_pulls_shape_check CHECK (
    (kind = 'backfill' AND event_id IS NOT NULL AND changed_since IS NULL)
    OR (kind = 'catchup' AND event_id IS NULL AND requested_by IS NULL AND changed_since IS NOT NULL)) NOT VALID;
