-- The Notification an Event's change of time or place is told by,
-- event.updated (iletişim-servisi 06, ADR 0070;
-- docs/event-updated-notification.md). While EVENT_UPDATED_NOTIFICATION is
-- on, the Event's update writes a row in its own transaction when a
-- published Event that is not over gets a new start, end or location. The
-- row gathers the changes of the next 15 minutes (one row per Event until a
-- worker first takes it) and keeps what the Event said before the first of
-- them, so a change undone within the window tells nobody. A worker sends it
-- to every Ticket holder and records each Ticket it reached, so a retry never
-- tells anyone twice.
--
-- Ids, times and the old place only, never an address or a name: the
-- holders are read when it is sent, so account erasure has nothing to do
-- here, and a Ticket deleted takes its record with it.
--
-- The release before this one keeps working on the new schema: it reads and
-- writes none of it.
SET LOCAL lock_timeout = '5s';

CREATE TABLE IF NOT EXISTS event_update_notifications (
    id UUID PRIMARY KEY,
    event_id UUID NOT NULL REFERENCES events (id) ON DELETE CASCADE,
    before_start_date TIMESTAMPTZ,
    before_end_date TIMESTAMPTZ,
    before_location TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    next_attempt_at TIMESTAMPTZ NOT NULL,
    started_at TIMESTAMPTZ,
    claimed_at TIMESTAMPTZ,
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0)
);

-- One row gathers an Event's changes until the worker first takes it.
CREATE UNIQUE INDEX IF NOT EXISTS event_update_notifications_gathering_idx
    ON event_update_notifications (event_id) WHERE started_at IS NULL;
CREATE INDEX IF NOT EXISTS event_update_notifications_due_idx ON event_update_notifications (next_attempt_at);
CREATE INDEX IF NOT EXISTS event_update_notifications_event_idx ON event_update_notifications (event_id, created_at);

CREATE TABLE IF NOT EXISTS event_update_notification_sends (
    notification_id UUID NOT NULL REFERENCES event_update_notifications (id) ON DELETE CASCADE,
    ticket_id UUID NOT NULL REFERENCES tickets (id) ON DELETE CASCADE,
    sent_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (notification_id, ticket_id)
);

CREATE INDEX IF NOT EXISTS event_update_notification_sends_ticket_idx ON event_update_notification_sends (ticket_id);
