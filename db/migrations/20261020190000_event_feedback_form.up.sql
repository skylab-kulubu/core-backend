-- The Event's feedback form (etkinlik geri bildirim formu, ticket 02;
-- Yusuf's decisions K1 of 2026-10-20): a form apart from the application
-- and extra forms, whose answers write no Ticket.
--
--   * feedback_mode: where feedback is asked (event: one form for the
--     Event; per_session: a form per Session, the Event's optional;
--     event_with_sessions: one form that asks about each Session).
--   * feedback_form_url, feedback_form_alias: the form and the short link
--     alias the Event gives it (empty: none).
--   * feedback_auto_send: the form is mailed when the Event ends (ticket
--     06 sends it).
--
-- The release before this one keeps working on the new schema: its writes
-- name their columns, so an Event it creates or updates takes or keeps the
-- defaults, and nothing it reads changes.
SET LOCAL lock_timeout = '5s';

ALTER TABLE events
    ADD COLUMN IF NOT EXISTS feedback_mode TEXT NOT NULL DEFAULT 'event',
    ADD COLUMN IF NOT EXISTS feedback_form_url TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS feedback_form_alias TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS feedback_auto_send BOOLEAN NOT NULL DEFAULT true;

ALTER TABLE events
    DROP CONSTRAINT IF EXISTS events_feedback_mode_check,
    ADD CONSTRAINT events_feedback_mode_check
        CHECK (feedback_mode IN ('event', 'per_session', 'event_with_sessions'));
