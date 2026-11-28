SET LOCAL lock_timeout = '5s';

-- Who wrote a door mark and what became of one taken back
-- (docs/venue-entry.md "Who wrote it", "Taking back").
--
-- ticket_checkins takes source and by_user_id, as venue_entries has: how
-- the Check-in was written (scan: the door scanned a credential; desk: the
-- door desk found the person; self: the person's own, the attendance QR,
-- check-in/me or a guest's) and the door staff who wrote it, when an
-- active account (a current-identity link that account erasure clears).
-- Rows written before this migration have neither: NULL, never guessed.
--
-- door_undos keeps a Check-in or a Venue entry the door took back (Deletion
-- lifecycle: a Check-in is not destroyed by an operator action). Undoing
-- moves the row here, with when and by whom, in the same transaction that
-- takes it out of its table; the live tables keep only what stands, so
-- their one-a-day and one-a-Session keys, and every reader of them, stay
-- as they are, and the Ticket can be checked in again (a new row). A row
-- here names nobody but door staff (current-identity links, cleared on
-- erasure); it lives as long as its Ticket, and the retention sweep
-- deletes it three years after it was taken back (door_undos_age).
--
-- The release before runs on the new schema: the new columns are nullable
-- and it neither reads nor writes them or door_undos (its undo deletes, as
-- before).
ALTER TABLE ticket_checkins ADD COLUMN IF NOT EXISTS source TEXT;
ALTER TABLE ticket_checkins ADD COLUMN IF NOT EXISTS by_user_id UUID REFERENCES users (id) ON DELETE SET NULL;
ALTER TABLE ticket_checkins DROP CONSTRAINT IF EXISTS ticket_checkins_source_check;
ALTER TABLE ticket_checkins ADD CONSTRAINT ticket_checkins_source_check
    CHECK (source IS NULL OR source IN ('scan', 'desk', 'self'));
COMMENT ON COLUMN ticket_checkins.source IS
    'How the Check-in was written: scan, desk or self; NULL for one written before 20261128120000.';
COMMENT ON COLUMN ticket_checkins.by_user_id IS
    'The door staff who wrote it, when an active account; account erasure clears it.';
-- The door staff's Check-ins: the door staff statistics.
CREATE INDEX IF NOT EXISTS ticket_checkins_by_user_idx ON ticket_checkins (by_user_id) WHERE by_user_id IS NOT NULL;

DROP TRIGGER IF EXISTS ticket_checkins_require_active_writer ON ticket_checkins;
CREATE TRIGGER ticket_checkins_require_active_writer
    BEFORE INSERT OR UPDATE OF by_user_id ON ticket_checkins
    FOR EACH ROW EXECUTE FUNCTION public.require_active_account_reference('by_user_id');

CREATE TABLE IF NOT EXISTS door_undos (
    id UUID PRIMARY KEY,
    kind TEXT NOT NULL,
    ticket_id UUID NOT NULL REFERENCES tickets (id) ON DELETE CASCADE,
    event_id UUID NOT NULL REFERENCES events (id) ON DELETE CASCADE,
    session_id UUID,
    event_day_id UUID,
    entry_date DATE,
    source TEXT,
    by_user_id UUID REFERENCES users (id) ON DELETE SET NULL,
    written_at TIMESTAMPTZ NOT NULL,
    scanned_at TIMESTAMPTZ,
    undone_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    undone_by UUID REFERENCES users (id) ON DELETE SET NULL,
    CONSTRAINT door_undos_kind_check CHECK (
        (kind = 'check_in' AND session_id IS NOT NULL AND entry_date IS NULL
            AND (source IS NULL OR source IN ('scan', 'desk', 'self')))
        OR (kind = 'entry' AND session_id IS NULL AND entry_date IS NOT NULL
            AND source IN ('scan', 'desk', 'attendance', 'walk_in')))
);
COMMENT ON TABLE door_undos IS
    'A Check-in or Venue entry the door took back (docs/venue-entry.md): the row as it was, when and by whom.';
CREATE INDEX IF NOT EXISTS door_undos_event_idx ON door_undos (event_id, undone_at);
CREATE INDEX IF NOT EXISTS door_undos_undone_at_idx ON door_undos (undone_at);

DROP TRIGGER IF EXISTS door_undos_require_active_writer ON door_undos;
CREATE TRIGGER door_undos_require_active_writer
    BEFORE INSERT OR UPDATE OF by_user_id ON door_undos
    FOR EACH ROW EXECUTE FUNCTION public.require_active_account_reference('by_user_id');
DROP TRIGGER IF EXISTS door_undos_require_active_undoer ON door_undos;
CREATE TRIGGER door_undos_require_active_undoer
    BEFORE INSERT OR UPDATE OF undone_by ON door_undos
    FOR EACH ROW EXECUTE FUNCTION public.require_active_account_reference('undone_by');
