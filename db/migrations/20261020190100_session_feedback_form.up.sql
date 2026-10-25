-- A Session's own feedback form (etkinlik geri bildirim formu, ticket 03;
-- Yusuf's decision K1 addendum of 2026-10-20): in an Event whose feedback
-- mode is per_session, each Session may name a form, mailed to whom checked
-- in to it after it ends. Its answers write no Ticket.
--
--   * feedback_form_url, feedback_form_alias: the form and the short link
--     alias the Event gives it (empty: none).
--   * feedback_auto_send: the form is mailed when the Session ends.
--
-- The release before this one keeps working on the new schema: its writes
-- name their columns, so a Session it creates or updates takes or keeps the
-- defaults, and nothing it reads changes.
SET LOCAL lock_timeout = '5s';

ALTER TABLE sessions
    ADD COLUMN IF NOT EXISTS feedback_form_url TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS feedback_form_alias TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS feedback_auto_send BOOLEAN NOT NULL DEFAULT true;
