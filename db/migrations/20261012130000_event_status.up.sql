-- Event status (ADR 0066): the one stored status of an Event, a draft only
-- its editors see, published, or cancelled (final). Whether a published
-- Event has ended is not stored; it follows from its dates. `active` is
-- derived from both from now on (internal/event/status.go) and the column
-- events.active is no longer read: it stays, untouched, so that an older
-- release rolled back to still works, and is dropped by a later migration.
--
-- Every existing Event is published, as of its creation: before the status
-- an Event was visible as soon as it was saved.
--
--   * published_at/by: the latest publication (the record of who did it).
--   * cancelled_at/by, cancel_reason: the cancellation; the reason is a
--     short public text.
--   * copied_from_event_id: the Event this one was copied from.
--
-- The release before this one keeps working on the new schema: its writes
-- name their columns, so a new Event it creates takes the defaults
-- (published), and nothing it reads changes.
-- The file runs as one transaction. Its ALTERs take events' strongest lock;
-- behind one waiting for a long transaction every Event read would queue.
-- Waiting at most 5 seconds instead, the migration fails, this release does
-- not start, the one before keeps serving, and the next start applies it.
SET LOCAL lock_timeout = '5s';

ALTER TABLE events
    ADD COLUMN IF NOT EXISTS status TEXT NOT NULL DEFAULT 'published',
    ADD COLUMN IF NOT EXISTS published_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS published_by UUID,
    ADD COLUMN IF NOT EXISTS cancelled_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS cancelled_by UUID,
    ADD COLUMN IF NOT EXISTS cancel_reason TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS copied_from_event_id UUID;

ALTER TABLE events
    DROP CONSTRAINT IF EXISTS events_status_check,
    ADD CONSTRAINT events_status_check CHECK (status IN ('draft', 'published', 'cancelled')),
    DROP CONSTRAINT IF EXISTS events_published_by_fkey,
    ADD CONSTRAINT events_published_by_fkey
        FOREIGN KEY (published_by) REFERENCES users (id) ON DELETE SET NULL,
    DROP CONSTRAINT IF EXISTS events_cancelled_by_fkey,
    ADD CONSTRAINT events_cancelled_by_fkey
        FOREIGN KEY (cancelled_by) REFERENCES users (id) ON DELETE SET NULL,
    DROP CONSTRAINT IF EXISTS events_copied_from_event_id_fkey,
    ADD CONSTRAINT events_copied_from_event_id_fkey
        FOREIGN KEY (copied_from_event_id) REFERENCES events (id) ON DELETE SET NULL;

UPDATE events SET published_at = created_at
WHERE status = 'published' AND published_at IS NULL;

CREATE INDEX IF NOT EXISTS events_current_status_idx ON events (status) WHERE archived_at IS NULL;

-- published_by and cancelled_by are current-identity links like archived_by
-- (account-lifecycle.md): only an active subject with no deletion marker may
-- be named, and account erasure clears them.
DROP TRIGGER IF EXISTS events_require_active_publisher ON events;
CREATE TRIGGER events_require_active_publisher
    BEFORE INSERT OR UPDATE OF published_by ON events
    FOR EACH ROW EXECUTE FUNCTION public.require_active_account_reference('published_by');

DROP TRIGGER IF EXISTS events_require_active_canceller ON events;
CREATE TRIGGER events_require_active_canceller
    BEFORE INSERT OR UPDATE OF cancelled_by ON events
    FOR EACH ROW EXECUTE FUNCTION public.require_active_account_reference('cancelled_by');
