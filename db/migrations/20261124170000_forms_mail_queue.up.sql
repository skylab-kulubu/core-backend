-- Forms' mails that core sends (ADR-0075 karar 5; docs/forms-mail.md): the
-- core.forms consumer writes one row per event and template in the
-- transaction that records the event as consumed (ADR-0071 karar 6); a
-- worker reads the content from Forms just before it sends (claim check),
-- posts it to SkyMail under a key of the event and removes the row.
--
-- A row holds ids and decision fields alone, never an address, a name, an
-- answer or a review note: those are read when it is sent, so a response
-- deleted meanwhile mails nobody. user_id is the member it goes to, when
-- the event names one: erasing the account removes the person's rows.
--
-- The release before this one keeps working on the new schema: it reads and
-- writes none of it.
SET LOCAL lock_timeout = '5s';

CREATE TABLE IF NOT EXISTS forms_mail_queue (
    event_source TEXT NOT NULL,
    event_id TEXT NOT NULL,
    template TEXT NOT NULL,
    event_type TEXT NOT NULL,
    user_id UUID,
    facts JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    claimed_at TIMESTAMPTZ,
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    -- The guest's copy was counted against the daily limit: a retry does
    -- not count it again.
    guest_counted BOOLEAN NOT NULL DEFAULT false,
    PRIMARY KEY (event_source, event_id, template)
);

CREATE INDEX IF NOT EXISTS forms_mail_queue_due_idx ON forms_mail_queue (next_attempt_at);
CREATE INDEX IF NOT EXISTS forms_mail_queue_user_idx ON forms_mail_queue (user_id) WHERE user_id IS NOT NULL;
