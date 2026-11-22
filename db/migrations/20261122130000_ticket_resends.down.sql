-- Rolls back sending a Ticket's mail again: what waits to be sent again
-- goes out once more under the first mail's key (SkyMail answers it as a
-- replay and sends nothing).
SET LOCAL lock_timeout = '5s';

DROP TABLE IF EXISTS ticket_resends;
ALTER TABLE ticket_notifications DROP COLUMN IF EXISTS resend;
