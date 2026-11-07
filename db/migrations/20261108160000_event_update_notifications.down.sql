-- Rolls back the Event change Notification queue; what waits in it is not sent.
DROP TABLE IF EXISTS event_update_notification_sends;
DROP TABLE IF EXISTS event_update_notifications;
