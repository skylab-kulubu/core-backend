DROP INDEX IF EXISTS urls_event_idx;
DROP INDEX IF EXISTS urls_current_form_idx;
ALTER TABLE urls
    DROP COLUMN IF EXISTS label,
    DROP COLUMN IF EXISTS event_id,
    DROP COLUMN IF EXISTS form_id;
