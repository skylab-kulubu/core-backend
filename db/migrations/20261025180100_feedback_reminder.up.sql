-- Feedback reminder (etkinlik geri bildirim formu, ticket 07; Yusuf's
-- decisions K4 and S3 of 2026-10-20): three days after a feedback mail
-- went, one reminder with the same links to whom has not answered, one mail
-- a person a day as the first one; never once the form closed.
--
--   * feedback_mails.kind 'reminder': a reminder mail, made from the
--     request mail it follows (reminder_planned_at marks the request whose
--     reminder was planned, once). sent marks a mail SkyMail took: only a
--     request that went gets a reminder.
--   * feedback_form_closings: when a feedback form closes (the forms
--     service's report, or now when it was closed); from then on its
--     invites' reminders are cancelled for good (feedback_invites.
--     reminder_cancelled_at). Opening the form again deletes the row and
--     takes back no cancellation.
--
-- A new column with a default, a new table and indexes: the release before
-- this one reads none of them, and writes no 'reminder' row.
SET LOCAL lock_timeout = '5s';

ALTER TABLE feedback_mails DROP CONSTRAINT IF EXISTS feedback_mails_kind_check;
ALTER TABLE feedback_mails ADD CONSTRAINT feedback_mails_kind_check CHECK (kind IN ('request', 'reminder'));
ALTER TABLE feedback_mails ADD COLUMN IF NOT EXISTS sent BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE feedback_mails ADD COLUMN IF NOT EXISTS reminder_planned_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS feedback_mails_remind_idx ON feedback_mails (done_at)
    WHERE kind = 'request' AND sent AND reminder_planned_at IS NULL;

CREATE INDEX IF NOT EXISTS feedback_invites_event_idx ON feedback_invites (event_id, session_id);

CREATE TABLE IF NOT EXISTS feedback_form_closings (
    form_id UUID PRIMARY KEY,
    closes_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
