-- Rolling back drops the door's mails' queue and the Event's two switches:
-- a mail not sent yet is not sent, and the release before reads neither.
DROP TABLE IF EXISTS attendance_mails;
ALTER TABLE events DROP CONSTRAINT IF EXISTS events_attendee_note_check;
ALTER TABLE events DROP COLUMN IF EXISTS attendee_note;
ALTER TABLE events DROP COLUMN IF EXISTS attendance_recorded_mail;
ALTER TABLE events DROP COLUMN IF EXISTS attendance_welcome_mail;
