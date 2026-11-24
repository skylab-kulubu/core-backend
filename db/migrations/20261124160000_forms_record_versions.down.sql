-- The release before reads no version: an event it handled is judged by the
-- Ticket rule alone, which is safe to repeat.
DROP TABLE IF EXISTS forms_record_versions;
