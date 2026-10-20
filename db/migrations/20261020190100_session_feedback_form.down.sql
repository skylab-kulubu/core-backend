ALTER TABLE sessions
    DROP COLUMN IF EXISTS feedback_auto_send,
    DROP COLUMN IF EXISTS feedback_form_alias,
    DROP COLUMN IF EXISTS feedback_form_url;
