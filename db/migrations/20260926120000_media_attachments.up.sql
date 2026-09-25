-- Media attachment and the Media lifecycle (ADR-0052, media redesign ticket
-- 02). A Media is pending until a Media attachment links it to a record,
-- attached while one does, and detached once the last one is removed.
-- Archive and purge stay on their own columns.
ALTER TABLE media
    ADD COLUMN IF NOT EXISTS status TEXT NOT NULL DEFAULT 'pending'
        CONSTRAINT media_status_check CHECK (status IN ('pending', 'attached', 'detached')),
    -- When a Media no attachment keeps is purged. NULL keeps it.
    ADD COLUMN IF NOT EXISTS expires_at TIMESTAMPTZ,
    -- Never written since the first migration; status replaces it.
    DROP COLUMN IF EXISTS attached;

-- The expiry cleanup walks these by id.
CREATE INDEX IF NOT EXISTS media_expiry_candidates_idx
    ON media (id)
    WHERE expires_at IS NOT NULL AND blob_purged_at IS NULL;

-- One row per link between a Media and the record that uses it, in core or
-- in another product. owner_service names the product that owns the record
-- ('core' for the links below).
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

-- Media stored before this migration. Whatever core links today becomes
-- attached with its Media attachment. The attachment triggers are created
-- below, after this: a record may still link a Media archived after it was
-- linked, and that link is kept like every other. A Media nothing links stays
-- pending with no expiry: the legacy backfill (media redesign ticket 08)
-- decides what happens to it. Every step is idempotent.
DROP TRIGGER IF EXISTS media_attachments_require_current_media ON media_attachments;
DROP TRIGGER IF EXISTS media_attachments_status ON media_attachments;

INSERT INTO media_attachments (media_id, owner_service, owner_type, owner_id, role)
SELECT linked.media_id, 'core', linked.owner_type, linked.owner_id, linked.role
FROM (
    SELECT cover_image_id AS media_id, 'event' AS owner_type, id AS owner_id, 'event_cover' AS role
    FROM events WHERE cover_image_id IS NOT NULL
    UNION ALL
    SELECT media_id, 'event', event_id, 'event_gallery' FROM event_images
    UNION ALL
    SELECT profile_picture_id, 'user', id, 'profile_picture' FROM users WHERE profile_picture_id IS NOT NULL
    UNION ALL
    SELECT unnest(certificate_layout_media_ids(draft_layout, NULL)), 'certificate_template', id, 'certificate_asset'
    FROM certificate_templates
    UNION ALL
    SELECT unnest(certificate_layout_media_ids(layout, asset_manifest)), 'certificate_template_version', id, 'certificate_asset'
    FROM certificate_template_versions
) linked
JOIN media ON media.id = linked.media_id
ON CONFLICT ON CONSTRAINT media_attachments_link_key DO NOTHING;

UPDATE media
SET status = 'attached', expires_at = NULL
WHERE (status <> 'attached' OR expires_at IS NOT NULL)
  AND EXISTS (SELECT 1 FROM media_attachments WHERE media_attachments.media_id = media.id);

-- A new attachment, like every other link, needs a current Media: not
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

DROP TRIGGER IF EXISTS media_attachments_require_current_media ON media_attachments;
CREATE TRIGGER media_attachments_require_current_media
    BEFORE INSERT OR UPDATE OF media_id ON media_attachments
    FOR EACH ROW EXECUTE FUNCTION require_current_attached_media();

-- The status follows the attachments: the first one makes the Media
-- attached, removing the last one detaches it for 30 days. The Media row is
-- locked first, so two transactions removing the last two attachments
-- cannot both see the other one still there.
CREATE OR REPLACE FUNCTION media_attachment_status()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF TG_OP IN ('UPDATE', 'DELETE') THEN
        PERFORM 1 FROM media WHERE id = OLD.media_id FOR UPDATE;
        UPDATE media
        SET status = 'detached', expires_at = now() + interval '30 days', updated_at = now()
        WHERE id = OLD.media_id
          AND status = 'attached'
          AND NOT EXISTS (SELECT 1 FROM media_attachments WHERE media_id = OLD.media_id);
    END IF;
    IF TG_OP IN ('INSERT', 'UPDATE') THEN
        PERFORM 1 FROM media WHERE id = NEW.media_id FOR UPDATE;
        UPDATE media
        SET status = 'attached', expires_at = NULL, updated_at = now()
        WHERE id = NEW.media_id
          AND (status <> 'attached' OR expires_at IS NOT NULL);
    END IF;
    RETURN NULL;
END;
$$;

DROP TRIGGER IF EXISTS media_attachments_status ON media_attachments;
CREATE TRIGGER media_attachments_status
    AFTER INSERT OR UPDATE OR DELETE ON media_attachments
    FOR EACH ROW EXECUTE FUNCTION media_attachment_status();

-- Core's own links write their Media attachments in the transaction that
-- writes the link, whoever writes it: an Event's cover and gallery, a
-- User's profile picture, a certificate template's draft and each published
-- version's assets. Account erasure's anonymization unlinks the
-- profile picture the same way.
CREATE OR REPLACE FUNCTION sync_core_media_attachments()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    owner_kind TEXT;
    link_role TEXT;
    old_owner UUID;
    new_owner UUID;
    old_ids UUID[] := ARRAY[]::UUID[];
    new_ids UUID[] := ARRAY[]::UUID[];
BEGIN
    CASE TG_TABLE_NAME
        WHEN 'events' THEN
            owner_kind := 'event';
            link_role := 'event_cover';
            IF TG_OP <> 'INSERT' THEN
                old_owner := OLD.id;
                old_ids := array_remove(ARRAY[OLD.cover_image_id], NULL);
            END IF;
            IF TG_OP <> 'DELETE' THEN
                new_owner := NEW.id;
                new_ids := array_remove(ARRAY[NEW.cover_image_id], NULL);
            END IF;
        WHEN 'event_images' THEN
            owner_kind := 'event';
            link_role := 'event_gallery';
            IF TG_OP <> 'INSERT' THEN
                old_owner := OLD.event_id;
                old_ids := ARRAY[OLD.media_id];
            END IF;
            IF TG_OP <> 'DELETE' THEN
                new_owner := NEW.event_id;
                new_ids := ARRAY[NEW.media_id];
            END IF;
        WHEN 'certificate_templates' THEN
            owner_kind := 'certificate_template';
            link_role := 'certificate_asset';
            IF TG_OP <> 'INSERT' THEN
                old_owner := OLD.id;
                old_ids := certificate_layout_media_ids(OLD.draft_layout, NULL);
            END IF;
            IF TG_OP <> 'DELETE' THEN
                new_owner := NEW.id;
                new_ids := certificate_layout_media_ids(NEW.draft_layout, NULL);
            END IF;
        WHEN 'certificate_template_versions' THEN
            owner_kind := 'certificate_template_version';
            link_role := 'certificate_asset';
            IF TG_OP <> 'INSERT' THEN
                old_owner := OLD.id;
                old_ids := certificate_layout_media_ids(OLD.layout, OLD.asset_manifest);
            END IF;
            IF TG_OP <> 'DELETE' THEN
                new_owner := NEW.id;
                new_ids := certificate_layout_media_ids(NEW.layout, NEW.asset_manifest);
            END IF;
        WHEN 'users' THEN
            owner_kind := 'user';
            link_role := 'profile_picture';
            IF TG_OP <> 'INSERT' THEN
                old_owner := OLD.id;
                old_ids := array_remove(ARRAY[OLD.profile_picture_id], NULL);
            END IF;
            IF TG_OP <> 'DELETE' THEN
                new_owner := NEW.id;
                new_ids := array_remove(ARRAY[NEW.profile_picture_id], NULL);
            END IF;
    END CASE;

    -- Only what changed: a link that stays is left alone, so a record still
    -- linking an archived Media can be saved as long as the link is not new.
    DELETE FROM media_attachments
    WHERE owner_service = 'core' AND owner_type = owner_kind AND owner_id = old_owner AND role = link_role
      AND media_id = ANY (old_ids)
      AND NOT (old_owner IS NOT DISTINCT FROM new_owner AND media_id = ANY (new_ids));
    INSERT INTO media_attachments (media_id, owner_service, owner_type, owner_id, role)
    SELECT DISTINCT linked, 'core', owner_kind, new_owner, link_role
    FROM unnest(new_ids) AS linked
    WHERE NOT (old_owner IS NOT DISTINCT FROM new_owner AND linked = ANY (old_ids))
    ON CONFLICT ON CONSTRAINT media_attachments_link_key DO NOTHING;
    RETURN NULL;
END;
$$;

DROP TRIGGER IF EXISTS events_sync_media_attachments ON events;
CREATE TRIGGER events_sync_media_attachments
    AFTER INSERT OR UPDATE OF cover_image_id OR DELETE ON events
    FOR EACH ROW EXECUTE FUNCTION sync_core_media_attachments();

DROP TRIGGER IF EXISTS event_images_sync_media_attachments ON event_images;
CREATE TRIGGER event_images_sync_media_attachments
    AFTER INSERT OR UPDATE OR DELETE ON event_images
    FOR EACH ROW EXECUTE FUNCTION sync_core_media_attachments();

DROP TRIGGER IF EXISTS users_sync_media_attachments ON users;
CREATE TRIGGER users_sync_media_attachments
    AFTER INSERT OR UPDATE OF profile_picture_id OR DELETE ON users
    FOR EACH ROW EXECUTE FUNCTION sync_core_media_attachments();

DROP TRIGGER IF EXISTS certificate_templates_sync_media_attachments ON certificate_templates;
CREATE TRIGGER certificate_templates_sync_media_attachments
    AFTER INSERT OR UPDATE OF draft_layout OR DELETE ON certificate_templates
    FOR EACH ROW EXECUTE FUNCTION sync_core_media_attachments();

DROP TRIGGER IF EXISTS certificate_template_versions_sync_media_attachments ON certificate_template_versions;
CREATE TRIGGER certificate_template_versions_sync_media_attachments
    AFTER INSERT OR UPDATE OF layout, asset_manifest OR DELETE ON certificate_template_versions
    FOR EACH ROW EXECUTE FUNCTION sync_core_media_attachments();
