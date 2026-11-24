-- Rolling back drops the reminder's tables: a planned reminder not sent yet
-- is not sent, and the release before this one never reads them.
DROP TABLE IF EXISTS event_reminder_mails;
DROP TABLE IF EXISTS event_reminders;
