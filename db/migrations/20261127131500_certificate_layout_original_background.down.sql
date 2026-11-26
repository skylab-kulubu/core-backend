-- Back to the Media ids of a layout without its converted background's
-- original (20260926120000). A link the up migration's definition made
-- stays until the record is written again.
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
        SELECT element->>'mediaId'
        FROM jsonb_array_elements(CASE WHEN jsonb_typeof(layout->'elements') = 'array' THEN layout->'elements' ELSE '[]'::JSONB END) element
        UNION ALL
        SELECT jsonb_object_keys(CASE WHEN jsonb_typeof(manifest) = 'object' THEN manifest ELSE '{}'::JSONB END)
    ) candidates
    WHERE candidate ~* '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
$$;
