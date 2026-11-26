SET LOCAL lock_timeout = '5s';

-- Rolling back keeps the new columns, their checks and their values: the
-- release before neither reads nor writes them (nullable, or with
-- defaults), and the next release reads them again (the up migration finds
-- them, ADD COLUMN IF NOT EXISTS). While the release before runs, a mapping
-- it saves keeps the details' questions as they were.
--
-- To remove them for good, by hand: ALTER TABLE event_form_mappings DROP
-- CONSTRAINT event_form_mappings_details_check, DROP COLUMN phone_question,
-- DROP COLUMN university_question, DROP COLUMN department_question, DROP
-- COLUMN grade_question; ALTER TABLE event_form_respondents DROP CONSTRAINT
-- event_form_respondents_details_check, DROP COLUMN phone_number, DROP
-- COLUMN university, DROP COLUMN department, DROP COLUMN grade;
SELECT 1;
