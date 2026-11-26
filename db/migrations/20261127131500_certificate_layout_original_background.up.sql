-- A converted PDF background keeps its original (certificate editor v2,
-- ticket 08; docs/certificate-template-errors.md, "Background text
-- conversion"): layout.backgroundOriginalMediaId names the PDF the text was
-- converted from. It is never printed, but the template links it, so the
-- editor can go back to it: the Media ids a layout links count it too, and
-- its Media attachment keeps it from being taken for an orphan and purged.
-- Only the function's body changes; the triggers that call it are as they
-- were.
SET LOCAL lock_timeout = '5s';

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
