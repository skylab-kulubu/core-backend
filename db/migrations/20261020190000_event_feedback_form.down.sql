ALTER TABLE events
    DROP CONSTRAINT IF EXISTS events_feedback_mode_check,
    DROP COLUMN IF EXISTS feedback_auto_send,
    DROP COLUMN IF EXISTS feedback_form_alias,
    DROP COLUMN IF EXISTS feedback_form_url,
    DROP COLUMN IF EXISTS feedback_mode;
