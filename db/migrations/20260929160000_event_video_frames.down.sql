-- Dropping the column would leave the frames' Media attachments behind (it
-- fires no statement trigger), and clearing the frames first would start
-- every frame's 30 days: refuse while any is left.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM event_videos WHERE frame_media_id IS NOT NULL)
        OR EXISTS (SELECT 1 FROM media_attachments WHERE owner_service = 'core' AND role = 'event_video_frame') THEN
        RAISE EXCEPTION 'event video frames down: videos still have frames; clear them first';
    END IF;
END;
$$;

DROP TRIGGER IF EXISTS event_videos_frame_attachments_insert ON event_videos;
DROP TRIGGER IF EXISTS event_videos_frame_attachments_update ON event_videos;
DROP TRIGGER IF EXISTS event_videos_frame_attachments_delete ON event_videos;
DROP FUNCTION IF EXISTS sync_event_video_frame_attachments();
DROP INDEX IF EXISTS event_videos_frame_due_idx;
DROP INDEX IF EXISTS event_videos_frame_media_id_idx;
ALTER TABLE event_videos DROP CONSTRAINT IF EXISTS event_videos_frame_check;
ALTER TABLE event_videos
    DROP COLUMN IF EXISTS frame_media_id,
    DROP COLUMN IF EXISTS frame_state,
    DROP COLUMN IF EXISTS frame_attempts,
    DROP COLUMN IF EXISTS frame_retry_at,
    DROP COLUMN IF EXISTS frame_claim_id,
    DROP COLUMN IF EXISTS frame_claimed_until;

-- 20260929140000's role tables.
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
