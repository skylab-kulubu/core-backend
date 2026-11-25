SET LOCAL lock_timeout = '5s';

-- Forms' decline of a Ticket's answer (Yusuf, K6 2026-11-25;
-- docs/form-response-tickets.md): core no longer cancels the Ticket a
-- declined answer wrote; it marks it, and the panel shows "Forms'ta
-- reddedildi" with a one-click cancel. NULL is no mark. A later acceptance
-- of the holder's answer clears it. A time only; the release before
-- neither reads nor writes it.
ALTER TABLE tickets ADD COLUMN IF NOT EXISTS forms_rejected_at TIMESTAMPTZ;
