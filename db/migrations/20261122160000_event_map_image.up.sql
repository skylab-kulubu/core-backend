-- An Event's map image (docs/event-map.md): when the Event's place gets a
-- point (20261122150000), core draws the map around it once, from the
-- club's own OpenStreetMap tiles, and keeps it as an event_map Media in the
-- Event's own column. The ticket PDF and the ticket page show it; a copy
-- with the same point shares it. A new point clears it and core draws
-- another; the old Media loses its Media attachment and expires as any
-- detached Media does.
--
-- Like a video's frame (20260929160000), a worker claims the Events that
-- wait for a map in a short transaction (map_claim_id, map_claimed_until),
-- backs off after a failure (map_attempts, map_retry_at) and gives up after
-- a few (map_state 'failed'): such an Event shows its place without a map.
-- A changed point starts over. map_retry_at also holds back a point just
-- set, a minute, so that a pin moved a few times is drawn once.
--
-- The release before this one keeps working on the new schema: it reads
-- and writes none of the columns. Its update of a point leaves the old map
-- in place until this release clears it; an Event's point is new with this
-- release's predecessor (20261122150000) only.
SET LOCAL lock_timeout = '5s';

ALTER TABLE events
    ADD COLUMN IF NOT EXISTS map_media_id UUID,
    ADD COLUMN IF NOT EXISTS map_state TEXT,
    ADD COLUMN IF NOT EXISTS map_attempts INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS map_retry_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS map_claim_id UUID,
    ADD COLUMN IF NOT EXISTS map_claimed_until TIMESTAMPTZ;

-- As a cover's: a Media row that goes takes only the map with it.
ALTER TABLE events DROP CONSTRAINT IF EXISTS events_map_media_id_fkey;
ALTER TABLE events ADD CONSTRAINT events_map_media_id_fkey
    FOREIGN KEY (map_media_id) REFERENCES media (id) ON DELETE SET NULL;

ALTER TABLE events
    DROP CONSTRAINT IF EXISTS events_map_check,
    ADD CONSTRAINT events_map_check CHECK (
        (map_state IS NULL OR map_state = 'failed')
        AND map_attempts >= 0
        AND (map_claim_id IS NULL) = (map_claimed_until IS NULL)
    );

-- The purge's lookups by Media.
CREATE INDEX IF NOT EXISTS events_map_media_id_idx
    ON events (map_media_id) WHERE map_media_id IS NOT NULL;

-- The worker walks the current Events with a point and no map, not given
-- up on.
CREATE INDEX IF NOT EXISTS events_map_due_idx
    ON events (id)
    WHERE map_media_id IS NULL AND map_state IS NULL AND location_lat IS NOT NULL AND archived_at IS NULL;

-- The map's Media attachment, written in the statement that writes the
-- link, as the cover's (20260926120000): the Event owns it in the role
-- event_map.
CREATE OR REPLACE TRIGGER events_map_attachments_insert
    AFTER INSERT ON events REFERENCING NEW TABLE AS new_owners
    FOR EACH STATEMENT EXECUTE FUNCTION sync_core_media_attachments('event', 'event_map', 'id', 'map_media_id');
CREATE OR REPLACE TRIGGER events_map_attachments_update
    AFTER UPDATE ON events REFERENCING OLD TABLE AS old_owners NEW TABLE AS new_owners
    FOR EACH STATEMENT EXECUTE FUNCTION sync_core_media_attachments('event', 'event_map', 'id', 'map_media_id');
CREATE OR REPLACE TRIGGER events_map_attachments_delete
    AFTER DELETE ON events REFERENCING OLD TABLE AS old_owners
    FOR EACH STATEMENT EXECUTE FUNCTION sync_core_media_attachments('event', 'event_map', 'id', 'map_media_id');

-- 20261004120000's copy of rolePurposes (internal/media/attachment.go),
-- with the map's role, which takes only event_map. A test keeps it equal
-- to rolePurposes, both ways.
CREATE OR REPLACE FUNCTION media_role_purposes()
RETURNS TABLE (owner_service TEXT, role TEXT, purpose TEXT)
LANGUAGE sql
IMMUTABLE
AS $$
    VALUES
        ('core', 'event_cover', 'event_cover'),
        ('core', 'event_cover', 'event_gallery'),
        ('core', 'event_gallery', 'event_gallery'),
        ('core', 'event_gallery', 'event_cover'),
        ('core', 'profile_picture', 'profile_picture'),
        ('core', 'certificate_asset', 'certificate_asset'),
        ('core', 'event_file', 'club_file'),
        ('core', 'event_video', 'video'),
        ('core', 'event_video_poster', 'event_cover'),
        ('core', 'event_video_poster', 'event_gallery'),
        ('core', 'event_video_frame', 'video_frame'),
        ('core', 'event_map', 'event_map'),
        ('forms', 'answer', 'answer_file'),
        ('forms', 'answer', 'answer_file_large'),
        ('forms', 'answer', 'answer_file_guest'),
        ('cms', 'image', 'cms_image'),
        ('cms', 'file', 'cms_file')
$$;

-- The roles a legacy Media does not fit (rolesWithoutLegacy): core draws an
-- Event's map itself, so no Media was ever linked as one without a
-- purpose. A test keeps the two equal.
CREATE OR REPLACE FUNCTION media_roles_without_legacy()
RETURNS TABLE (owner_service TEXT, role TEXT)
LANGUAGE sql
IMMUTABLE
AS $$
    VALUES
        ('core', 'event_file'),
        ('core', 'event_video'),
        ('core', 'event_video_poster'),
        ('core', 'event_video_frame'),
        ('core', 'event_map')
$$;
