-- The door's mode and Venue entry (kapi-postalari 01; docs/venue-entry.md).
--
-- events.entry_mode is what a door scan writes: per_session (a Check-in to
-- the Session picked, as before) or venue (a Venue entry for the day). NULL
-- is automatic: venue on an Event with no live Session, per_session on one
-- that has one. Every Event already there is automatic.
--
-- venue_entries is a Ticket's entry into the venue on a day: one a day
-- (ticket_id, entry_date). entry_date is the Europe/Istanbul day: the
-- Check-in's EventDay's, or the scan's when there is no EventDay. source is
-- how it was written: scan (the door scanned a credential), desk (the door
-- desk found the person) or attendance (a Session Check-in written without
-- one, check_in_id). It is never counted for a certificate. It lives as long
-- as its Ticket (ADR-0062) and names nobody: by_user_id is the door staff who
-- wrote it, a current-identity link that account erasure clears.
--
-- The release before runs on this schema: it reads and writes neither.
SET LOCAL lock_timeout = '5s';

ALTER TABLE events ADD COLUMN IF NOT EXISTS entry_mode TEXT;
ALTER TABLE events DROP CONSTRAINT IF EXISTS events_entry_mode_check;
ALTER TABLE events ADD CONSTRAINT events_entry_mode_check
    CHECK (entry_mode IS NULL OR entry_mode IN ('per_session', 'venue'));
COMMENT ON COLUMN events.entry_mode IS 'What a door scan writes: per_session or venue; NULL is automatic (venue without a live Session).';

CREATE TABLE IF NOT EXISTS venue_entries (
    id UUID PRIMARY KEY,
    ticket_id UUID NOT NULL REFERENCES tickets (id) ON DELETE CASCADE,
    event_id UUID NOT NULL REFERENCES events (id) ON DELETE CASCADE,
    entry_date DATE NOT NULL,
    event_day_id UUID REFERENCES event_days (id) ON DELETE SET NULL,
    source TEXT NOT NULL,
    check_in_id UUID REFERENCES ticket_checkins (id) ON DELETE SET NULL,
    by_user_id UUID REFERENCES users (id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT venue_entries_ticket_day_key UNIQUE (ticket_id, entry_date),
    CONSTRAINT venue_entries_source_check CHECK (source IN ('scan', 'desk', 'attendance'))
);
COMMENT ON TABLE venue_entries IS
    'Venue entry (docs/venue-entry.md): a Ticket entering the venue on a day, one a day; never counted for a certificate.';

-- The Event's entries of a day: the door's lists and counts.
CREATE INDEX IF NOT EXISTS venue_entries_event_day_idx ON venue_entries (event_id, entry_date);
-- The entry a Check-in wrote, which undoing the Check-in takes back.
CREATE INDEX IF NOT EXISTS venue_entries_check_in_idx ON venue_entries (check_in_id) WHERE check_in_id IS NOT NULL;

-- by_user_id is a current-identity link like events.published_by: only an
-- active subject with no deletion marker may be named.
DROP TRIGGER IF EXISTS venue_entries_require_active_writer ON venue_entries;
CREATE TRIGGER venue_entries_require_active_writer
    BEFORE INSERT OR UPDATE OF by_user_id ON venue_entries
    FOR EACH ROW EXECUTE FUNCTION public.require_active_account_reference('by_user_id');
