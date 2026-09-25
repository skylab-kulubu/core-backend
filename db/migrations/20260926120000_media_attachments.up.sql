-- Media attachment and the Media lifecycle (ADR-0052, media redesign ticket
-- 02). A Media is pending until a Media attachment links it to a record,
-- attached while one does, and detached once the last one is removed.
-- Archive and purge stay on their own columns.
ALTER TABLE media
    ADD COLUMN IF NOT EXISTS status TEXT NOT NULL DEFAULT 'pending'
        CONSTRAINT media_status_check CHECK (status IN ('pending', 'attached', 'detached')),
    -- When a Media no Media attachment keeps is purged. NULL keeps it.
    ADD COLUMN IF NOT EXISTS expires_at TIMESTAMPTZ,
    -- Never written since the first migration; status replaces it.
    DROP COLUMN IF EXISTS attached;

-- The expiry cleanup reads the Media whose expiry has come.
CREATE INDEX IF NOT EXISTS media_expiry_idx
    ON media (expires_at, id)
    WHERE expires_at IS NOT NULL AND blob_purged_at IS NULL;

-- One row per Media attachment: the link between a Media and the record that
-- uses it, in core or in another product. owner_service names the product
-- that owns the record ('core' for core's own links below).
CREATE TABLE IF NOT EXISTS media_attachments (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    media_id UUID NOT NULL REFERENCES media (id),
    owner_service TEXT NOT NULL CHECK (owner_service <> ''),
    owner_type TEXT NOT NULL CHECK (owner_type <> ''),
    owner_id UUID NOT NULL,
    role TEXT NOT NULL CHECK (role <> ''),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT media_attachments_link_key UNIQUE (media_id, owner_service, owner_type, owner_id, role)
);

CREATE INDEX IF NOT EXISTS media_attachments_owner_idx
    ON media_attachments (owner_service, owner_type, owner_id);

-- The Media ids a certificate layout (background, image elements) and a
-- published version's asset manifest name. Anything that is not a UUID is
-- left out; the current-media guards refuse it before this runs.
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

-- The core links one record (as JSON) holds: its owner id, read from
-- owner_column, and each Media named by media_column (a Media id, or a
-- certificate layout with, for a published version, its asset manifest in
-- manifest_column).
CREATE OR REPLACE FUNCTION core_media_links(record JSONB, owner_column TEXT, media_column TEXT, manifest_column TEXT)
RETURNS TABLE (owner_id UUID, media_id UUID)
LANGUAGE sql
IMMUTABLE
AS $$
    SELECT (record->>owner_column)::UUID, linked
    FROM unnest(CASE jsonb_typeof(record->media_column)
        WHEN 'string' THEN ARRAY[(record->>media_column)::UUID]
        WHEN 'object' THEN certificate_layout_media_ids(record->media_column, record->manifest_column)
        ELSE ARRAY[]::UUID[]
    END) AS linked
$$;

-- Media stored before this migration. Whatever core links today becomes
-- attached with its Media attachment. The Media attachment triggers are
-- created below, after this: a record may still link a Media archived after it
-- was linked, and that link is kept like every other. A Media nothing links
-- stays pending with no expiry: the legacy backfill (media redesign ticket 08)
-- decides what happens to it. Every step is idempotent.
DROP TRIGGER IF EXISTS media_attachments_require_current_media ON media_attachments;
DROP TRIGGER IF EXISTS media_attachments_status_insert ON media_attachments;
DROP TRIGGER IF EXISTS media_attachments_status_update ON media_attachments;
DROP TRIGGER IF EXISTS media_attachments_status_delete ON media_attachments;

INSERT INTO media_attachments (media_id, owner_service, owner_type, owner_id, role)
SELECT link.media_id, 'core', source.owner_type, link.owner_id, source.role
FROM (
    SELECT 'event', 'event_cover', to_jsonb(owner), 'id', 'cover_image_id', NULL::TEXT FROM events owner
    UNION ALL
    SELECT 'event', 'event_gallery', to_jsonb(owner), 'event_id', 'media_id', NULL FROM event_images owner
    UNION ALL
    SELECT 'user', 'profile_picture', to_jsonb(owner), 'id', 'profile_picture_id', NULL FROM users owner
    UNION ALL
    SELECT 'certificate_template', 'certificate_asset', to_jsonb(owner), 'id', 'draft_layout', NULL FROM certificate_templates owner
    UNION ALL
    SELECT 'certificate_template_version', 'certificate_asset', to_jsonb(owner), 'id', 'layout', 'asset_manifest'
    FROM certificate_template_versions owner
) source (owner_type, role, record, owner_column, media_column, manifest_column)
CROSS JOIN LATERAL core_media_links(source.record, source.owner_column, source.media_column, source.manifest_column) link
JOIN media ON media.id = link.media_id
ON CONFLICT ON CONSTRAINT media_attachments_link_key DO NOTHING;

UPDATE media
SET status = 'attached', expires_at = NULL
WHERE (status <> 'attached' OR expires_at IS NOT NULL)
  AND EXISTS (SELECT 1 FROM media_attachments WHERE media_attachments.media_id = media.id);

-- A new Media attachment, like every other link, needs a current Media: not
-- archived, and no purge started (20260919211000).
CREATE OR REPLACE FUNCTION require_current_attached_media()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM media
        WHERE id = NEW.media_id
          AND deleted_at IS NULL
          AND blob_purge_started_at IS NULL
          AND blob_purged_at IS NULL
    ) THEN
        RAISE EXCEPTION 'media % is not current', NEW.media_id USING ERRCODE = '23503';
    END IF;
    RETURN NEW;
END;
$$;

CREATE OR REPLACE TRIGGER media_attachments_require_current_media
    BEFORE INSERT OR UPDATE OF media_id ON media_attachments
    FOR EACH ROW EXECUTE FUNCTION require_current_attached_media();

-- The status follows the Media attachments, once per statement: a Media with
-- one is attached; a Media whose last one was removed is detached, and a
-- purposed one expires 30 days later. A legacy Media gets no expiry: it may
-- still be used outside core by its address, and only the legacy backfill
-- decides about it. The statement's Media rows are locked first, in id order
-- (so two statements never lock them the other way round), with a lock that
-- does not wait for foreign key references (so two links of one Media
-- written at once do not wait for each other); two transactions removing the
-- last two Media attachments then cannot both see the other one still there.
CREATE OR REPLACE FUNCTION media_attachment_status()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    touched UUID[];
BEGIN
    IF TG_OP = 'INSERT' THEN
        SELECT array_agg(DISTINCT media_id) INTO touched FROM new_attachments;
    ELSIF TG_OP = 'DELETE' THEN
        SELECT array_agg(DISTINCT media_id) INTO touched FROM old_attachments;
    ELSE
        SELECT array_agg(DISTINCT media_id) INTO touched
        FROM (SELECT media_id FROM old_attachments UNION SELECT media_id FROM new_attachments) moved;
    END IF;

    PERFORM 1 FROM media WHERE id = ANY (touched) ORDER BY id FOR NO KEY UPDATE;
    UPDATE media
    SET status = 'attached', expires_at = NULL, updated_at = now()
    WHERE id = ANY (touched)
      AND (status <> 'attached' OR expires_at IS NOT NULL)
      AND EXISTS (SELECT 1 FROM media_attachments a WHERE a.media_id = media.id);
    UPDATE media
    SET status = 'detached',
        expires_at = CASE WHEN purpose = 'legacy' THEN NULL ELSE now() + interval '30 days' END,
        updated_at = now()
    WHERE id = ANY (touched)
      AND status = 'attached'
      AND NOT EXISTS (SELECT 1 FROM media_attachments a WHERE a.media_id = media.id);
    RETURN NULL;
END;
$$;

CREATE OR REPLACE TRIGGER media_attachments_status_insert
    AFTER INSERT ON media_attachments REFERENCING NEW TABLE AS new_attachments
    FOR EACH STATEMENT EXECUTE FUNCTION media_attachment_status();
CREATE OR REPLACE TRIGGER media_attachments_status_update
    AFTER UPDATE ON media_attachments REFERENCING OLD TABLE AS old_attachments NEW TABLE AS new_attachments
    FOR EACH STATEMENT EXECUTE FUNCTION media_attachment_status();
CREATE OR REPLACE TRIGGER media_attachments_status_delete
    AFTER DELETE ON media_attachments REFERENCING OLD TABLE AS old_attachments
    FOR EACH STATEMENT EXECUTE FUNCTION media_attachment_status();

-- Core's own links write and remove their Media attachments in the statement
-- that writes the link, whoever writes it: an Event's cover and gallery, a
-- User's profile picture, a certificate template's draft and each published
-- version's assets. Account erasure's anonymization unlinks the profile
-- picture the same way. Each linking table passes its owner type, role and
-- columns (see core_media_links); only the links the statement changed are
-- written, so a record still linking a Media archived after it was linked can
-- be saved as long as that link stays.
CREATE OR REPLACE FUNCTION sync_core_media_attachments()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    owner_kind CONSTANT TEXT := TG_ARGV[0];
    link_role CONSTANT TEXT := TG_ARGV[1];
    before_links JSONB := '[]';
    after_links JSONB := '[]';
BEGIN
    IF TG_OP IN ('UPDATE', 'DELETE') THEN
        SELECT COALESCE(jsonb_agg(to_jsonb(link)), '[]') INTO before_links
        FROM old_owners owner
        CROSS JOIN LATERAL core_media_links(to_jsonb(owner), TG_ARGV[2], TG_ARGV[3], TG_ARGV[4]) link;
    END IF;
    IF TG_OP IN ('INSERT', 'UPDATE') THEN
        SELECT COALESCE(jsonb_agg(to_jsonb(link)), '[]') INTO after_links
        FROM new_owners owner
        CROSS JOIN LATERAL core_media_links(to_jsonb(owner), TG_ARGV[2], TG_ARGV[3], TG_ARGV[4]) link;
    END IF;

    DELETE FROM media_attachments a
    USING (
        SELECT * FROM jsonb_to_recordset(before_links) AS link (owner_id UUID, media_id UUID)
        EXCEPT
        SELECT * FROM jsonb_to_recordset(after_links) AS link (owner_id UUID, media_id UUID)
    ) gone
    WHERE a.owner_service = 'core' AND a.owner_type = owner_kind AND a.role = link_role
      AND a.owner_id = gone.owner_id AND a.media_id = gone.media_id;
    INSERT INTO media_attachments (media_id, owner_service, owner_type, owner_id, role)
    SELECT added.media_id, 'core', owner_kind, added.owner_id, link_role
    FROM (
        SELECT * FROM jsonb_to_recordset(after_links) AS link (owner_id UUID, media_id UUID)
        EXCEPT
        SELECT * FROM jsonb_to_recordset(before_links) AS link (owner_id UUID, media_id UUID)
    ) added
    ON CONFLICT ON CONSTRAINT media_attachments_link_key DO NOTHING;
    RETURN NULL;
END;
$$;

-- Transition tables allow one event per trigger: each linking table gets an
-- insert, an update and a delete trigger with the same arguments.
CREATE OR REPLACE TRIGGER events_media_attachments_insert
    AFTER INSERT ON events REFERENCING NEW TABLE AS new_owners
    FOR EACH STATEMENT EXECUTE FUNCTION sync_core_media_attachments('event', 'event_cover', 'id', 'cover_image_id');
CREATE OR REPLACE TRIGGER events_media_attachments_update
    AFTER UPDATE ON events REFERENCING OLD TABLE AS old_owners NEW TABLE AS new_owners
    FOR EACH STATEMENT EXECUTE FUNCTION sync_core_media_attachments('event', 'event_cover', 'id', 'cover_image_id');
CREATE OR REPLACE TRIGGER events_media_attachments_delete
    AFTER DELETE ON events REFERENCING OLD TABLE AS old_owners
    FOR EACH STATEMENT EXECUTE FUNCTION sync_core_media_attachments('event', 'event_cover', 'id', 'cover_image_id');

CREATE OR REPLACE TRIGGER event_images_media_attachments_insert
    AFTER INSERT ON event_images REFERENCING NEW TABLE AS new_owners
    FOR EACH STATEMENT EXECUTE FUNCTION sync_core_media_attachments('event', 'event_gallery', 'event_id', 'media_id');
CREATE OR REPLACE TRIGGER event_images_media_attachments_update
    AFTER UPDATE ON event_images REFERENCING OLD TABLE AS old_owners NEW TABLE AS new_owners
    FOR EACH STATEMENT EXECUTE FUNCTION sync_core_media_attachments('event', 'event_gallery', 'event_id', 'media_id');
CREATE OR REPLACE TRIGGER event_images_media_attachments_delete
    AFTER DELETE ON event_images REFERENCING OLD TABLE AS old_owners
    FOR EACH STATEMENT EXECUTE FUNCTION sync_core_media_attachments('event', 'event_gallery', 'event_id', 'media_id');

CREATE OR REPLACE TRIGGER users_media_attachments_insert
    AFTER INSERT ON users REFERENCING NEW TABLE AS new_owners
    FOR EACH STATEMENT EXECUTE FUNCTION sync_core_media_attachments('user', 'profile_picture', 'id', 'profile_picture_id');
CREATE OR REPLACE TRIGGER users_media_attachments_update
    AFTER UPDATE ON users REFERENCING OLD TABLE AS old_owners NEW TABLE AS new_owners
    FOR EACH STATEMENT EXECUTE FUNCTION sync_core_media_attachments('user', 'profile_picture', 'id', 'profile_picture_id');
CREATE OR REPLACE TRIGGER users_media_attachments_delete
    AFTER DELETE ON users REFERENCING OLD TABLE AS old_owners
    FOR EACH STATEMENT EXECUTE FUNCTION sync_core_media_attachments('user', 'profile_picture', 'id', 'profile_picture_id');

CREATE OR REPLACE TRIGGER certificate_templates_media_attachments_insert
    AFTER INSERT ON certificate_templates REFERENCING NEW TABLE AS new_owners
    FOR EACH STATEMENT EXECUTE FUNCTION sync_core_media_attachments('certificate_template', 'certificate_asset', 'id', 'draft_layout');
CREATE OR REPLACE TRIGGER certificate_templates_media_attachments_update
    AFTER UPDATE ON certificate_templates REFERENCING OLD TABLE AS old_owners NEW TABLE AS new_owners
    FOR EACH STATEMENT EXECUTE FUNCTION sync_core_media_attachments('certificate_template', 'certificate_asset', 'id', 'draft_layout');
CREATE OR REPLACE TRIGGER certificate_templates_media_attachments_delete
    AFTER DELETE ON certificate_templates REFERENCING OLD TABLE AS old_owners
    FOR EACH STATEMENT EXECUTE FUNCTION sync_core_media_attachments('certificate_template', 'certificate_asset', 'id', 'draft_layout');

CREATE OR REPLACE TRIGGER certificate_template_versions_media_attachments_insert
    AFTER INSERT ON certificate_template_versions REFERENCING NEW TABLE AS new_owners
    FOR EACH STATEMENT EXECUTE FUNCTION sync_core_media_attachments('certificate_template_version', 'certificate_asset', 'id', 'layout', 'asset_manifest');
CREATE OR REPLACE TRIGGER certificate_template_versions_media_attachments_update
    AFTER UPDATE ON certificate_template_versions REFERENCING OLD TABLE AS old_owners NEW TABLE AS new_owners
    FOR EACH STATEMENT EXECUTE FUNCTION sync_core_media_attachments('certificate_template_version', 'certificate_asset', 'id', 'layout', 'asset_manifest');
CREATE OR REPLACE TRIGGER certificate_template_versions_media_attachments_delete
    AFTER DELETE ON certificate_template_versions REFERENCING OLD TABLE AS old_owners
    FOR EACH STATEMENT EXECUTE FUNCTION sync_core_media_attachments('certificate_template_version', 'certificate_asset', 'id', 'layout', 'asset_manifest');
