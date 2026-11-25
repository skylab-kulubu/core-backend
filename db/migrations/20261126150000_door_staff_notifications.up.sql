SET LOCAL lock_timeout = '5s';

-- The mail a person added to an Event's door staff gets
-- (event.door-staff-added, docs/door-staff-mail.md). While DOOR_STAFF_MAIL
-- is on, the write that adds them to event_door_staff queues a row in the
-- same transaction; a worker sends it two minutes later, once the Event is
-- published, if they are still on the door staff. One row waits per Event
-- and person; the worker deletes it when it is done with.
--
-- Ids only. A users row deleted outright takes its rows; an erased
-- (anonymized) account's row is dropped by the worker, which reads the
-- account when it sends. The release before this one reads and writes none
-- of it.
CREATE TABLE IF NOT EXISTS door_staff_notifications (
    id UUID PRIMARY KEY,
    event_id UUID NOT NULL REFERENCES events (id) ON DELETE CASCADE,
    user_id UUID NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    next_attempt_at TIMESTAMPTZ NOT NULL,
    claimed_at TIMESTAMPTZ,
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    UNIQUE (event_id, user_id)
);

CREATE INDEX IF NOT EXISTS door_staff_notifications_due_idx ON door_staff_notifications (next_attempt_at);
CREATE INDEX IF NOT EXISTS door_staff_notifications_user_idx ON door_staff_notifications (user_id);
