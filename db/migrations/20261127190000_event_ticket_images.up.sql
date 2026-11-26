-- An Event's ticket image (docs/ticket-image.md): what the PDF ticket shows
-- in its band across the top. Without a row the band is cut from the
-- middle of the cover, as before. A row picks the cover's region
-- (source 'cover', cover_media_id the cover the region was drawn on: a
-- new cover goes back to the middle) or an image of its own (source
-- 'upload', media_id) and its region. The region is the image's own
-- fractions, 0 to 1: the image itself is never changed.
--
-- The uploaded image is an Event image (event_cover or event_gallery, as a
-- video's poster); the Event owns its Media attachment, in the role
-- event_ticket_image, written in the statement that writes the link
-- (20260926120000).
--
-- The release before this one keeps working on the new schema: it reads
-- and writes nothing of it.
SET LOCAL lock_timeout = '5s';

CREATE TABLE IF NOT EXISTS event_ticket_images (
    event_id UUID PRIMARY KEY REFERENCES events (id) ON DELETE CASCADE,
    source TEXT NOT NULL,
    -- As a cover's: a Media row that goes takes only the image with it;
    -- the band is then cut from the cover again.
    media_id UUID REFERENCES media (id) ON DELETE SET NULL,
    cover_media_id UUID,
    crop_x DOUBLE PRECISION NOT NULL,
    crop_y DOUBLE PRECISION NOT NULL,
    crop_w DOUBLE PRECISION NOT NULL,
    crop_h DOUBLE PRECISION NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT event_ticket_images_check CHECK (
        source IN ('cover', 'upload')
        AND (source = 'upload' OR media_id IS NULL)
        AND (source = 'cover' OR cover_media_id IS NULL)
        AND crop_x >= 0 AND crop_y >= 0 AND crop_w > 0 AND crop_h > 0
        AND crop_x + crop_w <= 1.000001 AND crop_y + crop_h <= 1.000001
    )
);

-- The purge's and the Team media library's lookups by Media.
CREATE INDEX IF NOT EXISTS event_ticket_images_media_id_idx
    ON event_ticket_images (media_id) WHERE media_id IS NOT NULL;

CREATE OR REPLACE TRIGGER event_ticket_images_attachments_insert
    AFTER INSERT ON event_ticket_images REFERENCING NEW TABLE AS new_owners
    FOR EACH STATEMENT EXECUTE FUNCTION sync_core_media_attachments('event', 'event_ticket_image', 'event_id', 'media_id');
CREATE OR REPLACE TRIGGER event_ticket_images_attachments_update
    AFTER UPDATE ON event_ticket_images REFERENCING OLD TABLE AS old_owners NEW TABLE AS new_owners
    FOR EACH STATEMENT EXECUTE FUNCTION sync_core_media_attachments('event', 'event_ticket_image', 'event_id', 'media_id');
CREATE OR REPLACE TRIGGER event_ticket_images_attachments_delete
    AFTER DELETE ON event_ticket_images REFERENCING OLD TABLE AS old_owners
    FOR EACH STATEMENT EXECUTE FUNCTION sync_core_media_attachments('event', 'event_ticket_image', 'event_id', 'media_id');

-- The role tables (rolePurposes, rolesWithoutLegacy in
-- internal/media/attachment.go) with the new role: it takes what a cover
-- takes, and no legacy Media (it was made after Media purpose). Each
-- function is written again from its own rows plus the new ones, so the
-- rows another migration of the same days added stay. A test keeps them
-- equal to the Go tables.
DO $migration$
DECLARE
    purpose_rows TEXT;
    strict_rows TEXT;
BEGIN
    SELECT string_agg(format('(%L, %L, %L)', r.owner_service, r.role, r.purpose), E',\n        ' ORDER BY r.n)
    INTO purpose_rows
    FROM (
        SELECT p.owner_service, p.role, p.purpose, p.n
        FROM media_role_purposes() WITH ORDINALITY AS p (owner_service, role, purpose, n)
        WHERE p.role <> 'event_ticket_image'
        UNION ALL
        SELECT 'core', 'event_ticket_image', 'event_cover', 1000001
        UNION ALL
        SELECT 'core', 'event_ticket_image', 'event_gallery', 1000002
    ) r;
    EXECUTE format($f$
CREATE OR REPLACE FUNCTION media_role_purposes()
RETURNS TABLE (owner_service TEXT, role TEXT, purpose TEXT)
LANGUAGE sql
IMMUTABLE
AS $body$
    VALUES
        %s
$body$$f$, purpose_rows);

    SELECT string_agg(format('(%L, %L)', r.owner_service, r.role), E',\n        ' ORDER BY r.n)
    INTO strict_rows
    FROM (
        SELECT s.owner_service, s.role, s.n
        FROM media_roles_without_legacy() WITH ORDINALITY AS s (owner_service, role, n)
        WHERE s.role <> 'event_ticket_image'
        UNION ALL
        SELECT 'core', 'event_ticket_image', 1000001
    ) r;
    EXECUTE format($f$
CREATE OR REPLACE FUNCTION media_roles_without_legacy()
RETURNS TABLE (owner_service TEXT, role TEXT)
LANGUAGE sql
IMMUTABLE
AS $body$
    VALUES
        %s
$body$$f$, strict_rows);
END
$migration$;
