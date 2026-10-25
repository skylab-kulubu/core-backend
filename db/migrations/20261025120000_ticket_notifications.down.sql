-- Rolls back the Ticket's Notification queue; what waits in it is not sent.
DROP TABLE IF EXISTS ticket_notifications;
