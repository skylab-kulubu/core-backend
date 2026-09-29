-- A video's frame (media redesign ticket 25, decision P1): when an Event's
-- video has no poster its organizers uploaded, core takes one frame of it
-- (the frame service, ffmpeg) and keeps it as a video_frame image Media, in
-- its own column: never in poster_media_id. The Event's detail shows the
-- uploaded poster while there is one, else the frame, so clearing the
-- uploaded poster falls back to the frame. Like the poster, the Event owns
-- its Media attachment, in the role event_video_frame, written in the
-- statement that writes the link.
ALTER TABLE event_videos
    ADD COLUMN IF NOT EXISTS frame_media_id UUID,
    -- NULL while the video waits for its frame (or has one); failed once
    -- core gave up on it: the frame service takes no frame from the video,
    -- or kept failing. A failed video is served with no poster.
    ADD COLUMN IF NOT EXISTS frame_state TEXT,
    -- The worker's backoff: tries that failed in a row, and when to try
    -- again.
    ADD COLUMN IF NOT EXISTS frame_attempts INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS frame_retry_at TIMESTAMPTZ,
    -- A worker's claim on the video, taken in a short transaction before it
    -- asks for the frame and let go when its step ends: only one worker (of
    -- the core replicas a rolling deploy runs side by side) takes a video's
    -- frame at a time.
    ADD COLUMN IF NOT EXISTS frame_claim_id UUID,
    ADD COLUMN IF NOT EXISTS frame_claimed_until TIMESTAMPTZ;

-- As the poster's: a Media row that goes (none does today) takes only the
-- frame with it, never the video.
ALTER TABLE event_videos DROP CONSTRAINT IF EXISTS event_videos_frame_media_id_fkey;
ALTER TABLE event_videos ADD CONSTRAINT event_videos_frame_media_id_fkey
    FOREIGN KEY (frame_media_id) REFERENCES media (id) ON DELETE SET NULL;

ALTER TABLE event_videos
    DROP CONSTRAINT IF EXISTS event_videos_frame_check,
    ADD CONSTRAINT event_videos_frame_check CHECK (
        (frame_state IS NULL OR frame_state = 'failed')
        AND frame_attempts >= 0
        AND (frame_claim_id IS NULL) = (frame_claimed_until IS NULL)
    );

-- The purge's lookups by Media.
CREATE INDEX IF NOT EXISTS event_videos_frame_media_id_idx
    ON event_videos (frame_media_id) WHERE frame_media_id IS NOT NULL;

-- The worker walks the videos that wait for a frame: none uploaded, none
-- taken, not given up on.
CREATE INDEX IF NOT EXISTS event_videos_frame_due_idx
    ON event_videos (event_id, media_id)
    WHERE frame_media_id IS NULL AND poster_media_id IS NULL AND frame_state IS NULL;

-- sync_event_video_poster_attachments (20260929140000) for the frames: the
-- same steps, on frame_media_id in the role event_video_frame. A frame is
-- one video's, but the function keeps a frame another video of the Event
-- still holds, as the poster's does, and locks the Events whose links the
-- statement changed (in id order, the lock core's own writers take before
-- they write) before it reads them. It is not shared with the poster's: that
-- one belongs to a migration whose fingerprint reads its source.
CREATE OR REPLACE FUNCTION sync_event_video_frame_attachments()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    before_links JSONB := '[]';
    after_links JSONB := '[]';
    gone_links JSONB;
    added_links JSONB;
BEGIN
    IF TG_OP IN ('UPDATE', 'DELETE') THEN
        SELECT COALESCE(jsonb_agg(to_jsonb(link)), '[]') INTO before_links
        FROM old_owners owner
        CROSS JOIN LATERAL core_media_links(to_jsonb(owner), 'event_id', 'frame_media_id', NULL) link;
    END IF;
    IF TG_OP IN ('INSERT', 'UPDATE') THEN
        SELECT COALESCE(jsonb_agg(to_jsonb(link)), '[]') INTO after_links
        FROM new_owners owner
        CROSS JOIN LATERAL core_media_links(to_jsonb(owner), 'event_id', 'frame_media_id', NULL) link;
    END IF;
    SELECT COALESCE(jsonb_agg(to_jsonb(gone)), '[]') INTO gone_links FROM (
        SELECT * FROM jsonb_to_recordset(before_links) AS link (owner_id UUID, media_id UUID)
        EXCEPT
        SELECT * FROM jsonb_to_recordset(after_links) AS link (owner_id UUID, media_id UUID)
    ) gone;
    SELECT COALESCE(jsonb_agg(to_jsonb(added)), '[]') INTO added_links FROM (
        SELECT * FROM jsonb_to_recordset(after_links) AS link (owner_id UUID, media_id UUID)
        EXCEPT
        SELECT * FROM jsonb_to_recordset(before_links) AS link (owner_id UUID, media_id UUID)
    ) added;
    IF gone_links = '[]' AND added_links = '[]' THEN
        RETURN NULL;
    END IF;

    PERFORM 1 FROM events
    WHERE id IN (SELECT link.owner_id FROM jsonb_to_recordset(gone_links || added_links) AS link (owner_id UUID, media_id UUID))
    ORDER BY id
    FOR NO KEY UPDATE;

    DELETE FROM media_attachments a
    USING jsonb_to_recordset(gone_links) AS gone (owner_id UUID, media_id UUID)
    WHERE a.owner_service = 'core' AND a.owner_type = 'event' AND a.role = 'event_video_frame'
      AND a.owner_id = gone.owner_id::TEXT AND a.media_id = gone.media_id
      AND NOT EXISTS (
        SELECT 1 FROM event_videos held
        WHERE held.event_id = gone.owner_id AND held.frame_media_id = gone.media_id
      );
    INSERT INTO media_attachments (media_id, owner_service, owner_type, owner_id, role)
    SELECT added.media_id, 'core', 'event', added.owner_id::TEXT, 'event_video_frame'
    FROM jsonb_to_recordset(added_links) AS added (owner_id UUID, media_id UUID)
    ON CONFLICT ON CONSTRAINT media_attachments_link_key DO NOTHING;
    RETURN NULL;
END;
$$;

CREATE OR REPLACE TRIGGER event_videos_frame_attachments_insert
    AFTER INSERT ON event_videos REFERENCING NEW TABLE AS new_owners
    FOR EACH STATEMENT EXECUTE FUNCTION sync_event_video_frame_attachments();
CREATE OR REPLACE TRIGGER event_videos_frame_attachments_update
    AFTER UPDATE ON event_videos REFERENCING OLD TABLE AS old_owners NEW TABLE AS new_owners
    FOR EACH STATEMENT EXECUTE FUNCTION sync_event_video_frame_attachments();
CREATE OR REPLACE TRIGGER event_videos_frame_attachments_delete
    AFTER DELETE ON event_videos REFERENCING OLD TABLE AS old_owners
    FOR EACH STATEMENT EXECUTE FUNCTION sync_event_video_frame_attachments();

-- 20260929140000's copy of rolePurposes (internal/media/attachment.go), with
-- the frame's role, which takes only video_frame. A test keeps it equal to
-- rolePurposes, both ways.
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
        ('forms', 'answer', 'answer_file'),
        ('forms', 'answer', 'answer_file_large'),
        ('cms', 'image', 'cms_image'),
        ('cms', 'file', 'cms_file')
$$;

-- The roles a legacy Media does not fit (rolesWithoutLegacy): core makes a
-- video's frame itself, so no Media was ever linked as one without a
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
        ('core', 'event_video_frame')
$$;
