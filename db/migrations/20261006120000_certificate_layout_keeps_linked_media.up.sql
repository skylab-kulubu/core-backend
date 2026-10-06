-- A certificate layout's current-media guard checks only the references a
-- write introduces (data-lifecycle ticket 03). A template whose draft links a
-- Media archived after it was linked can be edited as long as that link
-- stays, as an Event keeps an archived cover and a User an archived profile
-- picture (require_current_media_reference). Every reference an insert
-- makes, and every one an update adds or swaps in, must still be a current
-- Media: not archived, not being purged, not purged. A link that leaves the
-- layout and comes back is a new reference. The blob purger never claims a
-- Media a layout links, so a kept link never names a purging Media.
CREATE OR REPLACE FUNCTION require_current_certificate_layout_media()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    layout JSONB;
    manifest JSONB := '{}'::jsonb;
    old_layout JSONB := '{}'::jsonb;
    old_manifest JSONB := '{}'::jsonb;
    media_id TEXT;
BEGIN
    IF TG_TABLE_NAME = 'certificate_templates' THEN
        layout := NEW.draft_layout;
        IF TG_OP = 'UPDATE' THEN
            old_layout := OLD.draft_layout;
        END IF;
    ELSE
        layout := NEW.layout;
        manifest := NEW.asset_manifest;
        IF TG_OP = 'UPDATE' THEN
            old_layout := OLD.layout;
            old_manifest := OLD.asset_manifest;
        END IF;
    END IF;
    FOR media_id IN
        SELECT layout->>'backgroundMediaId'
        UNION ALL
        SELECT element->>'mediaId'
        FROM jsonb_array_elements(COALESCE(layout->'elements', '[]'::jsonb)) element
        UNION ALL
        SELECT jsonb_object_keys(COALESCE(manifest, '{}'::jsonb))
        EXCEPT
        (
            SELECT old_layout->>'backgroundMediaId'
            UNION ALL
            SELECT element->>'mediaId'
            FROM jsonb_array_elements(CASE WHEN jsonb_typeof(old_layout->'elements') = 'array' THEN old_layout->'elements' ELSE '[]'::jsonb END) element
            UNION ALL
            SELECT jsonb_object_keys(CASE WHEN jsonb_typeof(old_manifest) = 'object' THEN old_manifest ELSE '{}'::jsonb END)
        )
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
