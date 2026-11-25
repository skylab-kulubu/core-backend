-- Back to before certificate fonts (20261127183000.up): the table and its
-- Media attachments go (their files become detached and expire as any
-- detached Media does), the layout's Media ids leave fontMediaId out again
-- (20261127131500), and the role tables are 20261122160000's. A layout
-- link the up migration's definition made stays until the record is
-- written again.
SET LOCAL lock_timeout = '5s';

DROP TRIGGER IF EXISTS certificate_fonts_media_attachments_insert ON certificate_fonts;
DROP TRIGGER IF EXISTS certificate_fonts_media_attachments_update ON certificate_fonts;
DROP TRIGGER IF EXISTS certificate_fonts_media_attachments_delete ON certificate_fonts;
DROP TRIGGER IF EXISTS certificate_fonts_require_active_uploader ON certificate_fonts;

DELETE FROM media_attachments
WHERE owner_service = 'core' AND owner_type = 'certificate_font' AND role = 'certificate_font';

DROP TABLE IF EXISTS certificate_fonts;

CREATE OR REPLACE FUNCTION certificate_layout_media_ids(layout JSONB, manifest JSONB)
RETURNS UUID[]
LANGUAGE sql
IMMUTABLE
AS $$
    SELECT COALESCE(array_agg(DISTINCT candidate::UUID), ARRAY[]::UUID[])
    FROM (
        SELECT layout->>'backgroundMediaId' AS candidate
        UNION ALL
        SELECT layout->>'backgroundOriginalMediaId'
        UNION ALL
        SELECT element->>'mediaId'
        FROM jsonb_array_elements(CASE WHEN jsonb_typeof(layout->'elements') = 'array' THEN layout->'elements' ELSE '[]'::JSONB END) element
        UNION ALL
        SELECT jsonb_object_keys(CASE WHEN jsonb_typeof(manifest) = 'object' THEN manifest ELSE '{}'::JSONB END)
    ) candidates
    WHERE candidate ~* '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
$$;

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
