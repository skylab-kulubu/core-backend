-- Certificate fonts (certificate editor v2, ticket 18; docs/certificate-fonts.md):
-- a font a team, or the club (owner_team ''), uploads for its certificate
-- templates. pdf-tools checks the upload and writes it as WOFF2; core keeps
-- that file as a certificate_font Media (media_id), which this row links in
-- the role certificate_font. A layout's text element names it in
-- fontMediaId, and the layout links it too (certificate_layout_media_ids),
-- so a published version copies it like an image. Archiving a font
-- (archived_at) keeps its row and its file: drafts and versions that use it
-- go on printing it; only new links are refused.
--
-- The release before this one keeps working on the new schema: it reads
-- and writes none of it, and a layout it saves names no fontMediaId.
SET LOCAL lock_timeout = '5s';

CREATE TABLE IF NOT EXISTS certificate_fonts (
    id UUID PRIMARY KEY,
    -- '' is the club's: every template may use it.
    owner_team TEXT NOT NULL DEFAULT '',
    family TEXT NOT NULL CHECK (char_length(family) BETWEEN 1 AND 80),
    weight INTEGER NOT NULL CHECK (weight BETWEEN 1 AND 1000),
    style TEXT NOT NULL CHECK (style IN ('normal', 'italic')),
    -- The checked WOFF2.
    media_id UUID NOT NULL REFERENCES media (id),
    -- The Turkish letters (and basic Latin letters and digits) the font
    -- lacks, e.g. 'ğı'; '' when it has them all.
    missing_turkish TEXT NOT NULL DEFAULT '',
    uploaded_by UUID REFERENCES users (id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    archived_at TIMESTAMPTZ
);

-- One current face per family, weight and style in a team (or the club).
CREATE UNIQUE INDEX IF NOT EXISTS certificate_fonts_unique_face
    ON certificate_fonts (owner_team, lower(family), weight, style) WHERE archived_at IS NULL;

-- A team's current fonts (the list), and a layout's fonts by their file.
CREATE INDEX IF NOT EXISTS certificate_fonts_owner_team_idx
    ON certificate_fonts (owner_team) WHERE archived_at IS NULL;
CREATE INDEX IF NOT EXISTS certificate_fonts_media_id_idx
    ON certificate_fonts (media_id);

-- uploaded_by is a current-identity link like certificate_templates.created_by:
-- only an active subject with no deletion marker may be named. Erasure sets
-- it to NULL (user.AnonymizeAccount).
DROP TRIGGER IF EXISTS certificate_fonts_require_active_uploader ON certificate_fonts;
CREATE TRIGGER certificate_fonts_require_active_uploader
    BEFORE INSERT OR UPDATE OF uploaded_by ON certificate_fonts
    FOR EACH ROW EXECUTE FUNCTION public.require_active_account_reference('uploaded_by');

-- The font's Media attachment, written in the statement that writes the
-- row, as an Event file's (20260928160000).
CREATE OR REPLACE TRIGGER certificate_fonts_media_attachments_insert
    AFTER INSERT ON certificate_fonts REFERENCING NEW TABLE AS new_owners
    FOR EACH STATEMENT EXECUTE FUNCTION sync_core_media_attachments('certificate_font', 'certificate_font', 'id', 'media_id');
CREATE OR REPLACE TRIGGER certificate_fonts_media_attachments_update
    AFTER UPDATE ON certificate_fonts REFERENCING OLD TABLE AS old_owners NEW TABLE AS new_owners
    FOR EACH STATEMENT EXECUTE FUNCTION sync_core_media_attachments('certificate_font', 'certificate_font', 'id', 'media_id');
CREATE OR REPLACE TRIGGER certificate_fonts_media_attachments_delete
    AFTER DELETE ON certificate_fonts REFERENCING OLD TABLE AS old_owners
    FOR EACH STATEMENT EXECUTE FUNCTION sync_core_media_attachments('certificate_font', 'certificate_font', 'id', 'media_id');

-- The Media ids a layout links: 20261127131500's, and each element's
-- fontMediaId (backgroundOriginalMediaId stays).
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
        SELECT element->>'fontMediaId'
        FROM jsonb_array_elements(CASE WHEN jsonb_typeof(layout->'elements') = 'array' THEN layout->'elements' ELSE '[]'::JSONB END) element
        UNION ALL
        SELECT jsonb_object_keys(CASE WHEN jsonb_typeof(manifest) = 'object' THEN manifest ELSE '{}'::JSONB END)
    ) candidates
    WHERE candidate ~* '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
$$;

-- The role tables (media_role_purposes, media_roles_without_legacy; copies
-- of rolePurposes and rolesWithoutLegacy in internal/media/attachment.go,
-- which a test keeps equal): a font row links its file as certificate_font,
-- a layout links its fonts as certificate assets, and certificate_font takes
-- no legacy Media. Each function is written again from its current rows with
-- these added, never from a list of its own: migrations may be applied out
-- of order, and another one's role must not be lost.
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
        UNION
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
        UNION
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
