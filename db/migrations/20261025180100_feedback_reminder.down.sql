DROP TABLE IF EXISTS feedback_form_closings;
DROP INDEX IF EXISTS feedback_invites_event_idx;
DROP INDEX IF EXISTS feedback_mails_remind_idx;
DELETE FROM feedback_mails WHERE kind = 'reminder';
ALTER TABLE feedback_mails DROP COLUMN IF EXISTS reminder_planned_at;
ALTER TABLE feedback_mails DROP COLUMN IF EXISTS sent;
ALTER TABLE feedback_mails DROP CONSTRAINT IF EXISTS feedback_mails_kind_check;
ALTER TABLE feedback_mails ADD CONSTRAINT feedback_mails_kind_check CHECK (kind IN ('request'));
