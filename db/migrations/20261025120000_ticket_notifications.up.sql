-- The Notification a new Ticket is told by, event.ticket-created
-- (iletişim-servisi 06, ADR 0070; docs/ticket-created-notification.md).
-- The Ticket's insert writes the row in the same statement while
-- TICKET_CREATED_NOTIFICATION is on; a worker sends it to SkyMail and
-- removes it. A row holds the Ticket's id alone, never an address or a
-- name: those are read when it is sent, so a person erased meanwhile gets
-- nothing, and the row goes with its Ticket.
--
-- The release before this one keeps working on the new schema: it reads and
-- writes none of it.
SET LOCAL lock_timeout = '5s';

CREATE TABLE IF NOT EXISTS ticket_notifications (
    ticket_id UUID PRIMARY KEY REFERENCES tickets (id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    claimed_at TIMESTAMPTZ,
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0)
);

CREATE INDEX IF NOT EXISTS ticket_notifications_due_idx ON ticket_notifications (next_attempt_at);
