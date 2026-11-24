-- The reminder the day before an Event (kontenjan-iptal-hatirlatma 08,
-- SPEC §3.5; docs/event-reminder.md): at 18:00 Istanbul time the day before
-- a published Event starts, everyone holding an active Ticket of it gets one
-- mail, event.reminder.
--
--   * event_reminders: an Event's reminder, planned once per Istanbul day
--     of its start (starts_on). A start moved to another day earns a new
--     one; a start moved within the same day does not (event.updated tells
--     that change).
--   * event_reminder_mails: the queue, one row per active Ticket at
--     planning. Ids only: the address is read when the mail goes. The rows
--     of one address go as one mail. attempted_at is the last time the row
--     led a send: the worker counts them for its per-minute bound.
--
-- New tables only: the release before this one never reads them.
SET LOCAL lock_timeout = '5s';

CREATE TABLE IF NOT EXISTS event_reminders (
    event_id UUID NOT NULL REFERENCES events (id) ON DELETE CASCADE,
    starts_on DATE NOT NULL,
    starts_at TIMESTAMPTZ NOT NULL,
    planned_at TIMESTAMPTZ NOT NULL,
    tickets INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (event_id, starts_on)
);

CREATE TABLE IF NOT EXISTS event_reminder_mails (
    id UUID PRIMARY KEY,
    event_id UUID NOT NULL REFERENCES events (id) ON DELETE CASCADE,
    ticket_id UUID NOT NULL REFERENCES tickets (id) ON DELETE CASCADE,
    starts_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    next_attempt_at TIMESTAMPTZ NOT NULL,
    attempts INTEGER NOT NULL DEFAULT 0,
    claimed_at TIMESTAMPTZ,
    attempted_at TIMESTAMPTZ,
    done_at TIMESTAMPTZ,
    outcome TEXT CHECK (outcome IN ('sent', 'dropped', 'skipped', 'rejected', 'expired', 'uncertain')),
    CONSTRAINT event_reminder_mails_key UNIQUE (ticket_id, starts_at)
);

CREATE INDEX IF NOT EXISTS event_reminder_mails_due_idx ON event_reminder_mails (next_attempt_at) WHERE done_at IS NULL;
CREATE INDEX IF NOT EXISTS event_reminder_mails_event_idx ON event_reminder_mails (event_id, starts_at) WHERE done_at IS NULL;
CREATE INDEX IF NOT EXISTS event_reminder_mails_attempted_idx ON event_reminder_mails (attempted_at) WHERE attempted_at IS NOT NULL;
