-- A video's poster (media redesign ticket 24, decision P1): an image Media
-- the Event's organizers chose for one of its videos, of a purpose an Event
-- cover takes (event_cover or event_gallery). Like the cover, the gallery,
-- the files and the videos, the Event owns its Media attachment, in the
-- role event_video_poster, written in the statement that writes the link.
ALTER TABLE event_videos ADD COLUMN IF NOT EXISTS poster_media_id UUID;

-- As the cover's: a Media row that goes (none does today) takes only the
-- poster with it, never the video.
ALTER TABLE event_videos DROP CONSTRAINT IF EXISTS event_videos_poster_media_id_fkey;
ALTER TABLE event_videos ADD CONSTRAINT event_videos_poster_media_id_fkey
    FOREIGN KEY (poster_media_id) REFERENCES media (id) ON DELETE SET NULL;

-- The purge's and the Team media library's lookups by Media.
CREATE INDEX IF NOT EXISTS event_videos_poster_media_id_idx
    ON event_videos (poster_media_id) WHERE poster_media_id IS NOT NULL;

-- sync_core_media_attachments (20260926121000) for the posters: statement
-- triggers read the links the statement changed from its transition
-- tables and write only those. One thing differs: the Event owns every
-- video's poster link, and two of its videos may show the same image, so a
-- link one video gave up keeps its Media attachment while another video of
-- the Event still holds it (read after the statement, in the table itself).
-- A new link is always offered, so the Media attachment guards check it
-- (current Media, purpose fit) even when another video holds it already.
CREATE OR REPLACE FUNCTION sync_event_video_poster_attachments()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    before_links JSONB := '[]';
    after_links JSONB := '[]';
BEGIN
    IF TG_OP IN ('UPDATE', 'DELETE') THEN
        SELECT COALESCE(jsonb_agg(to_jsonb(link)), '[]') INTO before_links
        FROM old_owners owner
        CROSS JOIN LATERAL core_media_links(to_jsonb(owner), 'event_id', 'poster_media_id', NULL) link;
    END IF;
    IF TG_OP IN ('INSERT', 'UPDATE') THEN
        SELECT COALESCE(jsonb_agg(to_jsonb(link)), '[]') INTO after_links
        FROM new_owners owner
        CROSS JOIN LATERAL core_media_links(to_jsonb(owner), 'event_id', 'poster_media_id', NULL) link;
    END IF;

    DELETE FROM media_attachments a
    USING (
        SELECT * FROM jsonb_to_recordset(before_links) AS link (owner_id UUID, media_id UUID)
        EXCEPT
        SELECT * FROM jsonb_to_recordset(after_links) AS link (owner_id UUID, media_id UUID)
    ) gone
    WHERE a.owner_service = 'core' AND a.owner_type = 'event' AND a.role = 'event_video_poster'
      AND a.owner_id = gone.owner_id::TEXT AND a.media_id = gone.media_id
      AND NOT EXISTS (
        SELECT 1 FROM event_videos held
        WHERE held.event_id = gone.owner_id AND held.poster_media_id = gone.media_id
      );
    INSERT INTO media_attachments (media_id, owner_service, owner_type, owner_id, role)
    SELECT added.media_id, 'core', 'event', added.owner_id::TEXT, 'event_video_poster'
    FROM (
        SELECT * FROM jsonb_to_recordset(after_links) AS link (owner_id UUID, media_id UUID)
        EXCEPT
        SELECT * FROM jsonb_to_recordset(before_links) AS link (owner_id UUID, media_id UUID)
    ) added
    ON CONFLICT ON CONSTRAINT media_attachments_link_key DO NOTHING;
    RETURN NULL;
END;
$$;

CREATE OR REPLACE TRIGGER event_videos_poster_attachments_insert
    AFTER INSERT ON event_videos REFERENCING NEW TABLE AS new_owners
    FOR EACH STATEMENT EXECUTE FUNCTION sync_event_video_poster_attachments();
CREATE OR REPLACE TRIGGER event_videos_poster_attachments_update
    AFTER UPDATE ON event_videos REFERENCING OLD TABLE AS old_owners NEW TABLE AS new_owners
    FOR EACH STATEMENT EXECUTE FUNCTION sync_event_video_poster_attachments();
CREATE OR REPLACE TRIGGER event_videos_poster_attachments_delete
    AFTER DELETE ON event_videos REFERENCING OLD TABLE AS old_owners
    FOR EACH STATEMENT EXECUTE FUNCTION sync_event_video_poster_attachments();

-- 20260928160000's copy of rolePurposes (internal/media/attachment.go),
-- with the poster's role, which takes what an Event cover takes. A test
-- keeps it equal to rolePurposes, both ways.
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
        ('forms', 'answer', 'answer_file'),
        ('forms', 'answer', 'answer_file_large'),
        ('cms', 'image', 'cms_image'),
        ('cms', 'file', 'cms_file')
$$;

-- The roles a legacy Media does not fit (rolesWithoutLegacy): a poster came
-- after Media purpose, like an Event's files and videos, so no Media was
-- ever linked as one without a purpose. A test keeps the two equal.
CREATE OR REPLACE FUNCTION media_roles_without_legacy()
RETURNS TABLE (owner_service TEXT, role TEXT)
LANGUAGE sql
IMMUTABLE
AS $$
    VALUES
        ('core', 'event_file'),
        ('core', 'event_video'),
        ('core', 'event_video_poster')
$$;
