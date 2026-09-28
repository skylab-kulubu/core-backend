-- Dropping the link tables would leave their Media attachments behind (a
-- DROP fires no statement trigger), and removing the links first would start
-- every file's and video's 30 days: refuse while any is left.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM event_files) OR EXISTS (SELECT 1 FROM event_videos)
        OR EXISTS (SELECT 1 FROM media_attachments WHERE owner_service = 'core' AND role IN ('event_file', 'event_video')) THEN
        RAISE EXCEPTION 'event files down: Events still hold files or videos; remove them first';
    END IF;
END;
$$;

DROP TABLE IF EXISTS event_videos;
DROP TABLE IF EXISTS event_files;

-- 20260926161000's role table and purpose check.
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
        ('forms', 'answer', 'answer_file'),
        ('forms', 'answer', 'answer_file_large'),
        ('cms', 'image', 'cms_image'),
        ('cms', 'file', 'cms_file')
$$;

CREATE OR REPLACE FUNCTION media_purpose_fits_role(owner_service TEXT, role TEXT, purpose TEXT)
RETURNS BOOLEAN
LANGUAGE sql
IMMUTABLE
AS $$
    SELECT purpose = 'legacy' OR EXISTS (
        SELECT 1 FROM media_role_purposes() fitting
        WHERE fitting.owner_service = media_purpose_fits_role.owner_service
          AND fitting.role = media_purpose_fits_role.role
          AND fitting.purpose = media_purpose_fits_role.purpose
    )
$$;

DROP FUNCTION IF EXISTS media_roles_without_legacy();
