-- The door's mails (kapi-postalari 04, 05; docs/attendance-mail.md): a
-- Ticket's holder hears "hoş geldin" when they first enter the venue on a
-- day and "katılımın alındı" on each Session Check-in.
--
--   * events.attendance_welcome_mail and attendance_recorded_mail: the
--     organizer's switch for each mail on the Event, on by default (Yusuf,
--     2026-11-24 S1/S2). The person cannot turn them off: they are
--     transactional mails of their own scan.
--   * events.attendee_note: the organizer's note for whoever enters (the
--     venue's Wi-Fi, the way to the room), plain text up to 1000
--     characters, empty for none. Only the welcome mail and who may edit
--     the Event read it: never a public answer (Yusuf, 2026-11-24 S3).
--   * attendance_mails: the queue, ids only. While ATTENDANCE_MAIL is on, a
--     welcome row is written in the statement that writes the day's Venue
--     entry, a recorded row in the one that writes the Check-in, due two
--     minutes later. One row a Ticket a day (welcome), one a Ticket a
--     Session (recorded): a second scan, and a scan after an undo, queue
--     nothing new, so nobody is told twice. A row that was dropped
--     (undone before it went) is queued again by the next scan.
--     attempted_at is the last time the row led a send: the worker counts
--     them for its per-minute bound.
--
-- The release before runs on the new schema: it writes no row and reads
-- neither column (both have defaults).
SET LOCAL lock_timeout = '5s';

ALTER TABLE events ADD COLUMN IF NOT EXISTS attendance_welcome_mail BOOLEAN NOT NULL DEFAULT true;
ALTER TABLE events ADD COLUMN IF NOT EXISTS attendance_recorded_mail BOOLEAN NOT NULL DEFAULT true;
COMMENT ON COLUMN events.attendance_welcome_mail IS 'Whether the door''s welcome mail goes for the Event (docs/attendance-mail.md).';
ALTER TABLE events ADD COLUMN IF NOT EXISTS attendee_note TEXT NOT NULL DEFAULT '';
ALTER TABLE events DROP CONSTRAINT IF EXISTS events_attendee_note_check;
ALTER TABLE events ADD CONSTRAINT events_attendee_note_check CHECK (char_length(attendee_note) <= 1000);
COMMENT ON COLUMN events.attendee_note IS 'The organizer''s note for whoever enters (Wi-Fi, the room): the welcome mail and the editors alone read it.';
COMMENT ON COLUMN events.attendance_recorded_mail IS 'Whether the door''s attendance receipt goes for the Event (docs/attendance-mail.md).';

CREATE TABLE IF NOT EXISTS attendance_mails (
    id UUID PRIMARY KEY,
    kind TEXT NOT NULL,
    ticket_id UUID NOT NULL REFERENCES tickets (id) ON DELETE CASCADE,
    event_id UUID NOT NULL REFERENCES events (id) ON DELETE CASCADE,
    entry_date DATE NOT NULL,
    session_id UUID REFERENCES sessions (id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    next_attempt_at TIMESTAMPTZ NOT NULL,
    attempts INTEGER NOT NULL DEFAULT 0,
    claimed_at TIMESTAMPTZ,
    attempted_at TIMESTAMPTZ,
    done_at TIMESTAMPTZ,
    outcome TEXT,
    CONSTRAINT attendance_mails_kind_check CHECK (
        (kind = 'welcome' AND session_id IS NULL) OR (kind = 'recorded' AND session_id IS NOT NULL)),
    CONSTRAINT attendance_mails_outcome_check CHECK (
        outcome IS NULL OR outcome IN ('sent', 'merged', 'dropped', 'skipped', 'rejected', 'expired', 'uncertain'))
);
COMMENT ON TABLE attendance_mails IS
    'The door''s mails waiting or done (docs/attendance-mail.md): ids only, one welcome a Ticket a day, one receipt a Ticket a Session.';

CREATE UNIQUE INDEX IF NOT EXISTS attendance_mails_welcome_key ON attendance_mails (ticket_id, entry_date) WHERE kind = 'welcome';
CREATE UNIQUE INDEX IF NOT EXISTS attendance_mails_recorded_key ON attendance_mails (ticket_id, session_id) WHERE kind = 'recorded';
CREATE INDEX IF NOT EXISTS attendance_mails_due_idx ON attendance_mails (next_attempt_at) WHERE done_at IS NULL;
CREATE INDEX IF NOT EXISTS attendance_mails_ticket_day_idx ON attendance_mails (ticket_id, entry_date) WHERE done_at IS NULL;
CREATE INDEX IF NOT EXISTS attendance_mails_attempted_idx ON attendance_mails (attempted_at) WHERE attempted_at IS NOT NULL;
CREATE INDEX IF NOT EXISTS attendance_mails_done_idx ON attendance_mails (done_at) WHERE done_at IS NOT NULL;
