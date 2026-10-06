-- Back to checking every reference a layout holds on each write
-- (20260919211000).
CREATE OR REPLACE FUNCTION require_current_certificate_layout_media()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    layout JSONB;
    manifest JSONB := '{}'::jsonb;
    media_id TEXT;
BEGIN
    IF TG_TABLE_NAME = 'certificate_templates' THEN
        layout := NEW.draft_layout;
    ELSE
        layout := NEW.layout;
        manifest := NEW.asset_manifest;
    END IF;
    FOR media_id IN
        SELECT layout->>'backgroundMediaId'
        UNION ALL
        SELECT element->>'mediaId'
        FROM jsonb_array_elements(COALESCE(layout->'elements', '[]'::jsonb)) element
        UNION ALL
        SELECT jsonb_object_keys(COALESCE(manifest, '{}'::jsonb))
    LOOP
        IF COALESCE(media_id, '') <> '' AND NOT EXISTS (
            SELECT 1 FROM media
            WHERE id::text = media_id
              AND deleted_at IS NULL
              AND blob_purge_started_at IS NULL
              AND blob_purged_at IS NULL
        ) THEN
            RAISE EXCEPTION 'media % is not current', media_id USING ERRCODE = '23503';
        END IF;
    END LOOP;
    RETURN NEW;
END;
$$;
