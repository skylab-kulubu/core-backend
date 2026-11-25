SET LOCAL lock_timeout = '5s';

-- Forms' decline of a Ticket's answer (Yusuf, K6 2026-11-25;
-- docs/form-response-tickets.md): core no longer cancels the Ticket a
-- declined answer wrote; it marks it, and the panel shows "Forms'ta
-- reddedildi" with a one-click cancel. NULL is no mark. A later acceptance
-- of the holder's answer clears it.
--
-- form_response_form_id and form_response_status are the form of the
-- Ticket's forms answer (form_response_id) and where Forms has it: a newer
-- accepted answer takes the Ticket over only when it is of the same form
-- and the Ticket's answer is no longer accepted (pending or declined). NULL
-- on a Ticket whose answer was kept before this migration: never taken
-- over. Ids, a status and a time only; the release before neither reads
-- nor writes them.
ALTER TABLE tickets ADD COLUMN IF NOT EXISTS forms_rejected_at TIMESTAMPTZ;
ALTER TABLE tickets ADD COLUMN IF NOT EXISTS form_response_form_id UUID;
ALTER TABLE tickets ADD COLUMN IF NOT EXISTS form_response_status TEXT;
ALTER TABLE tickets DROP CONSTRAINT IF EXISTS tickets_form_response_status_check;
ALTER TABLE tickets ADD CONSTRAINT tickets_form_response_status_check
    CHECK (form_response_status IS NULL OR form_response_status IN ('accepted', 'pending', 'declined'));
