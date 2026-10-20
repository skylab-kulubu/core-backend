-- Rolls back the Event status mails: the queue and its record go, and so
-- does the mark of a notified cancellation.
DROP TABLE IF EXISTS event_status_mail_sends;
DROP TABLE IF EXISTS event_status_mails;
ALTER TABLE events DROP COLUMN IF EXISTS cancel_notified_at;
