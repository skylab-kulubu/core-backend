-- The mails that tell an Event's Ticket holders of its cancellation, and of
-- its cancellation undone (ADR 0066 addendum 2026-10-19, decision 1; ticket
-- 14). Queued in the same transaction as the cancellation, sent by a
-- worker that reads the recipients when it sends: a row holds ids, never an
-- address or a name, so account erasure has nothing to do here.
--
--   * events.cancel_notified_at: the current cancellation's mail is queued;
--     undoing it offers "Katılımcılara tekrar bildir" by it, and clears it.
--   * event_status_mails: one row per Event and mail. A reinstated mail
--     follows the cancellation mail it answers, and goes only to whom that
--     one reached. A row is kept once done (done_at), so that the
--     reinstated mail can read who got the cancellation.
--   * event_status_mail_sends: the Tickets a mail reached, so that a retry
--     never writes to anyone twice.
--
-- The release before this one keeps working on the new schema: it reads and
-- writes none of it.
SET LOCAL lock_timeout = '5s';

ALTER TABLE events ADD COLUMN IF NOT EXISTS cancel_notified_at TIMESTAMPTZ;

CREATE TABLE IF NOT EXISTS event_status_mails (
    id UUID PRIMARY KEY,
    event_id UUID NOT NULL REFERENCES events (id) ON DELETE CASCADE,
    kind TEXT NOT NULL CHECK (kind IN ('cancelled', 'reinstated')),
    follows_mail_id UUID REFERENCES event_status_mails (id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    claimed_at TIMESTAMPTZ,
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    done_at TIMESTAMPTZ,
    CONSTRAINT event_status_mails_follows_check CHECK ((kind = 'reinstated') = (follows_mail_id IS NOT NULL))
);

CREATE INDEX IF NOT EXISTS event_status_mails_due_idx ON event_status_mails (next_attempt_at) WHERE done_at IS NULL;
CREATE INDEX IF NOT EXISTS event_status_mails_event_idx ON event_status_mails (event_id, created_at);

CREATE TABLE IF NOT EXISTS event_status_mail_sends (
    mail_id UUID NOT NULL REFERENCES event_status_mails (id) ON DELETE CASCADE,
    ticket_id UUID NOT NULL REFERENCES tickets (id) ON DELETE CASCADE,
    sent_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (mail_id, ticket_id)
);

CREATE INDEX IF NOT EXISTS event_status_mail_sends_ticket_idx ON event_status_mail_sends (ticket_id);
