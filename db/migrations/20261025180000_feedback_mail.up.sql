-- Feedback mail (etkinlik geri bildirim formu, ticket 06; Yusuf's
-- decisions K2, K3 and S2 of 2026-10-20): the feedback form of an Event or
-- Session is mailed, with each person's own invite link, to whom checked
-- in, two hours after it ends, or at once with "Şimdi gönder".
--
--   * feedback_mail_targets: a target (the Event, or one Session) whose
--     form was sent, once: by the schedule (trigger auto) or by hand
--     (manual). A target here is never sent again.
--   * feedback_mails: the queue, one mail per Ticket. The schedule's mail
--     gathers a person's targets of one EventDay (event_day_id) and goes
--     two hours after the last Session they checked in to that day ends; a
--     manual mail carries its one target. Ids only: the address is read
--     when the mail is sent.
--   * feedback_mail_items: the targets one mail carries.
--
-- New tables only: the release before this one never reads them.
SET LOCAL lock_timeout = '5s';

CREATE TABLE IF NOT EXISTS feedback_mail_targets (
    event_id UUID NOT NULL REFERENCES events (id) ON DELETE CASCADE,
    session_id UUID REFERENCES sessions (id) ON DELETE CASCADE,
    form_id UUID NOT NULL,
    trigger TEXT NOT NULL CHECK (trigger IN ('auto', 'manual')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT feedback_mail_targets_key UNIQUE NULLS NOT DISTINCT (event_id, session_id)
);

CREATE TABLE IF NOT EXISTS feedback_mails (
    id UUID PRIMARY KEY,
    event_id UUID NOT NULL REFERENCES events (id) ON DELETE CASCADE,
    ticket_id UUID NOT NULL REFERENCES tickets (id) ON DELETE CASCADE,
    event_day_id UUID REFERENCES event_days (id) ON DELETE SET NULL,
    kind TEXT NOT NULL DEFAULT 'request' CHECK (kind IN ('request')),
    manual BOOLEAN NOT NULL DEFAULT false,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    next_attempt_at TIMESTAMPTZ NOT NULL,
    attempts INTEGER NOT NULL DEFAULT 0,
    claimed_at TIMESTAMPTZ,
    done_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS feedback_mails_due_idx ON feedback_mails (next_attempt_at) WHERE done_at IS NULL;
CREATE INDEX IF NOT EXISTS feedback_mails_ticket_idx ON feedback_mails (ticket_id) WHERE done_at IS NULL;

CREATE TABLE IF NOT EXISTS feedback_mail_items (
    mail_id UUID NOT NULL REFERENCES feedback_mails (id) ON DELETE CASCADE,
    event_id UUID NOT NULL REFERENCES events (id) ON DELETE CASCADE,
    session_id UUID REFERENCES sessions (id) ON DELETE CASCADE,
    CONSTRAINT feedback_mail_items_key UNIQUE NULLS NOT DISTINCT (mail_id, event_id, session_id)
);
