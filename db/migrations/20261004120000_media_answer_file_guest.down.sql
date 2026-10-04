-- Guest Answer files would be left with a role table that no longer names
-- their purpose, and staged uploads of no one with no subject: refuse while
-- any is left.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM media WHERE purpose = 'answer_file_guest')
        OR EXISTS (SELECT 1 FROM media_upload_staging WHERE subject_id IS NULL) THEN
        RAISE EXCEPTION 'media answer_file_guest down: guest Answer files or their staged uploads are left; remove them first';
    END IF;
END;
$$;

ALTER TABLE media_upload_staging ALTER COLUMN subject_id SET NOT NULL;

-- 20260929160000's role table.
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
