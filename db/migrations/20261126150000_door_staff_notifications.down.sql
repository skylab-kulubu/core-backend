SET LOCAL lock_timeout = '5s';

-- Rolls back the door staff mail queue; what waits in it is not sent. The
-- release before neither writes nor reads it.
DROP TABLE IF EXISTS door_staff_notifications;
