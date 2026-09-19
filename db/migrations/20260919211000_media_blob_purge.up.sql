ALTER TABLE media
    ADD COLUMN IF NOT EXISTS blob_purge_started_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS blob_purged_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS blob_purge_checked_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS media_blob_purge_candidates_idx
    ON media (deleted_at, id)
    WHERE deleted_at IS NOT NULL AND blob_purged_at IS NULL;

CREATE OR REPLACE FUNCTION require_current_media_reference()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    media_id UUID;
    changed BOOLEAN := true;
BEGIN
    CASE TG_TABLE_NAME
        WHEN 'events' THEN
            media_id := NEW.cover_image_id;
            IF TG_OP = 'UPDATE' THEN
                changed := OLD.cover_image_id IS DISTINCT FROM NEW.cover_image_id;
            END IF;
        WHEN 'event_images' THEN
            media_id := NEW.media_id;
        WHEN 'users' THEN
            media_id := NEW.profile_picture_id;
            IF TG_OP = 'UPDATE' THEN
                changed := OLD.profile_picture_id IS DISTINCT FROM NEW.profile_picture_id;
            END IF;
    END CASE;
    IF changed AND media_id IS NOT NULL AND NOT EXISTS (
        SELECT 1 FROM media
        WHERE id = media_id
          AND deleted_at IS NULL
          AND blob_purge_started_at IS NULL
          AND blob_purged_at IS NULL
    ) THEN
        RAISE EXCEPTION 'media % is not current', media_id USING ERRCODE = '23503';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER events_require_current_cover_media
    BEFORE INSERT OR UPDATE OF cover_image_id ON events
    FOR EACH ROW EXECUTE FUNCTION require_current_media_reference();

CREATE TRIGGER event_images_require_current_media
    BEFORE INSERT OR UPDATE OF media_id ON event_images
    FOR EACH ROW EXECUTE FUNCTION require_current_media_reference();

CREATE TRIGGER users_require_current_profile_media
    BEFORE INSERT OR UPDATE OF profile_picture_id ON users
    FOR EACH ROW EXECUTE FUNCTION require_current_media_reference();

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

CREATE TRIGGER certificate_templates_require_current_media
    BEFORE INSERT OR UPDATE OF draft_layout ON certificate_templates
    FOR EACH ROW EXECUTE FUNCTION require_current_certificate_layout_media();

CREATE TRIGGER certificate_template_versions_require_current_media
    BEFORE INSERT OR UPDATE OF layout, asset_manifest ON certificate_template_versions
    FOR EACH ROW EXECUTE FUNCTION require_current_certificate_layout_media();
