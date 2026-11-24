-- A Ticket's cancellation mail and the forms answer it came from
-- (kontenjan-iptal-hatirlatma 04, 05; docs/ticket-status-mail.md,
-- docs/form-response-tickets.md).
--
-- ticket_notifications.kind takes cancelled: "Biletin iptal edildi"
-- (event.ticket-cancelled), queued in the transaction that cancels the
-- Ticket while TICKET_STATUS_MAIL is on, when its holder cancels it or an
-- operator cancels it and chooses to tell.
--
-- tickets.form_response_id is the forms answer (Forms' responseId) whose
-- accepted report wrote or reopened the Ticket: that answer's decline
-- cancels it, and nothing else does. Every Ticket already there, and one
-- any other way writes, has none; one reopened another way loses it.
--
-- The release before runs on the new schema: it writes neither (its rows
-- are created, its Tickets keep no answer), and its worker would send a
-- queued cancelled row as "Biletin hazır" (see the down migration).
SET LOCAL lock_timeout = '5s';

ALTER TABLE ticket_notifications DROP CONSTRAINT IF EXISTS ticket_notifications_kind_check;
ALTER TABLE ticket_notifications ADD CONSTRAINT ticket_notifications_kind_check
    CHECK (kind IN ('created', 'waitlisted', 'promoted', 'cancelled'));

ALTER TABLE tickets ADD COLUMN IF NOT EXISTS form_response_id UUID;
COMMENT ON COLUMN tickets.form_response_id IS 'The forms answer whose accepted report wrote or reopened the Ticket; its decline cancels it.';
CREATE INDEX IF NOT EXISTS tickets_form_response_idx ON tickets (form_response_id)
    WHERE form_response_id IS NOT NULL;
