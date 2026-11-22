-- Sending a Ticket's mail again and renewing its link (ADR 0082;
-- bilet-qr-ve-pdf ticket 05; docs/ticket-resend.md).
--
-- ticket_resends is each time the panel sent a Ticket's mail again: the
-- n-th of the Ticket (its Notification's key is ...:r<n>), whether the link
-- was renewed with it, when, and the hash of the client's Idempotency-Key,
-- so a repeated request writes no second row. It holds no person: who did
-- it is not kept, and the row goes with its Ticket. The hour's count bounds
-- how often a Ticket is sent again.
--
-- ticket_notifications.resend is which of them a queued row sends: 0 is the
-- Ticket's first mail (key ...:v1).
--
-- The release before this one keeps working on the new schema: it writes
-- resend 0 by default and never reads ticket_resends.
SET LOCAL lock_timeout = '5s';

ALTER TABLE ticket_notifications
    ADD COLUMN IF NOT EXISTS resend INTEGER NOT NULL DEFAULT 0
        CONSTRAINT ticket_notifications_resend_check CHECK (resend >= 0);

CREATE TABLE IF NOT EXISTS ticket_resends (
    ticket_id UUID NOT NULL REFERENCES tickets (id) ON DELETE CASCADE,
    n INTEGER NOT NULL CHECK (n >= 1),
    renewed BOOLEAN NOT NULL DEFAULT false,
    request_hash TEXT CHECK (request_hash IS NULL OR request_hash ~ '^[0-9a-f]{64}$'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (ticket_id, n)
);

CREATE UNIQUE INDEX IF NOT EXISTS ticket_resends_request_idx
    ON ticket_resends (ticket_id, request_hash) WHERE request_hash IS NOT NULL;
CREATE INDEX IF NOT EXISTS ticket_resends_recent_idx ON ticket_resends (ticket_id, created_at);
