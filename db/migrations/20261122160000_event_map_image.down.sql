-- Rolls back an Event's map image. The maps' Media attachments go with the
-- column (the trigger's statement is dropped first, so the rows are deleted
-- here); their Media become detached and expire. The role tables go back to
-- 20261004120000's.
SET LOCAL lock_timeout = '5s';

DROP TRIGGER IF EXISTS events_map_attachments_insert ON events;
DROP TRIGGER IF EXISTS events_map_attachments_update ON events;
DROP TRIGGER IF EXISTS events_map_attachments_delete ON events;
DELETE FROM media_attachments WHERE owner_service = 'core' AND owner_type = 'event' AND role = 'event_map';
DROP INDEX IF EXISTS events_map_due_idx;
DROP INDEX IF EXISTS events_map_media_id_idx;
ALTER TABLE events
    DROP CONSTRAINT IF EXISTS events_map_check,
    DROP CONSTRAINT IF EXISTS events_map_media_id_fkey,
    DROP COLUMN IF EXISTS map_claimed_until,
    DROP COLUMN IF EXISTS map_claim_id,
    DROP COLUMN IF EXISTS map_retry_at,
    DROP COLUMN IF EXISTS map_attempts,
    DROP COLUMN IF EXISTS map_state,
    DROP COLUMN IF EXISTS map_media_id;

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
        ('forms', 'answer', 'answer_file_guest'),
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
        ('core', 'event_video_poster'),
        ('core', 'event_video_frame')
$$;
