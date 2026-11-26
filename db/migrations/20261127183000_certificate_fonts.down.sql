-- Back to before certificate fonts (20261127183000.up): the table and its
-- Media attachments go (their files become detached and expire as any
-- detached Media does), the layout's Media ids leave fontMediaId out again
-- (20261127131500), and the role tables lose this migration's rows. A layout
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

-- The role tables lose this migration's rows only, written again from
-- their current rows (another migration's role stays).
DO $roles$
DECLARE
    pairs TEXT;
    strict_roles TEXT;
BEGIN
    SELECT string_agg(format('(%L, %L, %L)', t.owner_service, t.role, t.purpose), E',\n        '
               ORDER BY t.owner_service, t.role, t.purpose)
    INTO pairs
    FROM (
        SELECT f.owner_service, f.role, f.purpose FROM media_role_purposes() f
        EXCEPT
        VALUES ('core', 'certificate_asset', 'certificate_font'), ('core', 'certificate_font', 'certificate_font')
    ) t (owner_service, role, purpose);
    EXECUTE format($f$CREATE OR REPLACE FUNCTION media_role_purposes()
RETURNS TABLE (owner_service TEXT, role TEXT, purpose TEXT)
LANGUAGE sql
IMMUTABLE
AS $body$
    VALUES
        %s
$body$$f$, pairs);

    SELECT string_agg(format('(%L, %L)', t.owner_service, t.role), E',\n        ' ORDER BY t.owner_service, t.role)
    INTO strict_roles
    FROM (
        SELECT f.owner_service, f.role FROM media_roles_without_legacy() f
        EXCEPT
        VALUES ('core', 'certificate_font')
    ) t (owner_service, role);
    EXECUTE format($f$CREATE OR REPLACE FUNCTION media_roles_without_legacy()
RETURNS TABLE (owner_service TEXT, role TEXT)
LANGUAGE sql
IMMUTABLE
AS $body$
    VALUES
        %s
$body$$f$, strict_roles);
END
$roles$;
