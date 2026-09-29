-- Dropping the column would leave the posters' Media attachments behind (it
-- fires no statement trigger), and clearing the posters first would start
-- every poster's 30 days: refuse while any is left.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM event_videos WHERE poster_media_id IS NOT NULL)
        OR EXISTS (SELECT 1 FROM media_attachments WHERE owner_service = 'core' AND role = 'event_video_poster') THEN
        RAISE EXCEPTION 'event video posters down: videos still have posters; clear them first';
    END IF;
END;
$$;

DROP TRIGGER IF EXISTS event_videos_poster_attachments_insert ON event_videos;
DROP TRIGGER IF EXISTS event_videos_poster_attachments_update ON event_videos;
DROP TRIGGER IF EXISTS event_videos_poster_attachments_delete ON event_videos;
DROP FUNCTION IF EXISTS sync_event_video_poster_attachments();
DROP INDEX IF EXISTS event_videos_poster_media_id_idx;
ALTER TABLE event_videos DROP COLUMN IF EXISTS poster_media_id;

-- 20260928160000's role tables.
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
        ('forms', 'answer', 'answer_file'),
        ('forms', 'answer', 'answer_file_large'),
        ('cms', 'image', 'cms_image'),
        ('cms', 'file', 'cms_file')
$$;

CREATE OR REPLACE FUNCTION media_roles_without_legacy()
RETURNS TABLE (owner_service TEXT, role TEXT)
LANGUAGE sql
IMMUTABLE
AS $$
    VALUES
        ('core', 'event_file'),
        ('core', 'event_video')
$$;
