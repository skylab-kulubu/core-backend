-- An Event's files and videos (media redesign ticket 22, decision C1): club
-- downloads (club_file: PDF or ZIP) and recordings (video: MP4), both sent by
-- Direct upload, in the order the Event's organizers give them. Like the
-- gallery (event_images), each link table's statement triggers write and
-- remove the Media attachments of its core role, event_file or event_video,
-- in the statement that writes the link (20260926120000).
CREATE TABLE IF NOT EXISTS event_files (
    event_id UUID NOT NULL REFERENCES events (id) ON DELETE CASCADE,
    media_id UUID NOT NULL REFERENCES media (id) ON DELETE CASCADE,
    -- The organizers' order; ties (two files added at once by two
    -- organizers) are read in added_at and then media_id order.
    order_index INTEGER NOT NULL,
    added_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (event_id, media_id)
);

CREATE INDEX IF NOT EXISTS event_files_media_id_idx ON event_files (media_id);

CREATE TABLE IF NOT EXISTS event_videos (
    event_id UUID NOT NULL REFERENCES events (id) ON DELETE CASCADE,
    media_id UUID NOT NULL REFERENCES media (id) ON DELETE CASCADE,
    order_index INTEGER NOT NULL,
    added_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (event_id, media_id)
);

CREATE INDEX IF NOT EXISTS event_videos_media_id_idx ON event_videos (media_id);

CREATE OR REPLACE TRIGGER event_files_media_attachments_insert
    AFTER INSERT ON event_files REFERENCING NEW TABLE AS new_owners
    FOR EACH STATEMENT EXECUTE FUNCTION sync_core_media_attachments('event', 'event_file', 'event_id', 'media_id');
CREATE OR REPLACE TRIGGER event_files_media_attachments_update
    AFTER UPDATE ON event_files REFERENCING OLD TABLE AS old_owners NEW TABLE AS new_owners
    FOR EACH STATEMENT EXECUTE FUNCTION sync_core_media_attachments('event', 'event_file', 'event_id', 'media_id');
CREATE OR REPLACE TRIGGER event_files_media_attachments_delete
    AFTER DELETE ON event_files REFERENCING OLD TABLE AS old_owners
    FOR EACH STATEMENT EXECUTE FUNCTION sync_core_media_attachments('event', 'event_file', 'event_id', 'media_id');

CREATE OR REPLACE TRIGGER event_videos_media_attachments_insert
    AFTER INSERT ON event_videos REFERENCING NEW TABLE AS new_owners
    FOR EACH STATEMENT EXECUTE FUNCTION sync_core_media_attachments('event', 'event_video', 'event_id', 'media_id');
CREATE OR REPLACE TRIGGER event_videos_media_attachments_update
    AFTER UPDATE ON event_videos REFERENCING OLD TABLE AS old_owners NEW TABLE AS new_owners
    FOR EACH STATEMENT EXECUTE FUNCTION sync_core_media_attachments('event', 'event_video', 'event_id', 'media_id');
CREATE OR REPLACE TRIGGER event_videos_media_attachments_delete
    AFTER DELETE ON event_videos REFERENCING OLD TABLE AS old_owners
    FOR EACH STATEMENT EXECUTE FUNCTION sync_core_media_attachments('event', 'event_video', 'event_id', 'media_id');

-- 20260926161000's copy of rolePurposes (internal/media/attachment.go), with
-- core's two new roles. A test keeps it equal to rolePurposes, both ways.
CREATE OR REPLACE FUNCTION media_role_purposes()
RETURNS TABLE (owner_service TEXT, role TEXT, purpose TEXT)
LANGUAGE sql
IMMUTABLE
AS $$
    VALUES
        ('core', 'event_cover', 'event_cover'),
        ('core', 'event_cover', 'event_gallery'),
        ('core', 'event_gallery', 'event_gallery'),
        ('core', 'event_gallery', 'event_cover'),
        ('core', 'profile_picture', 'profile_picture'),
        ('core', 'certificate_asset', 'certificate_asset'),
        ('core', 'event_file', 'club_file'),
        ('core', 'event_video', 'video'),
        ('forms', 'answer', 'answer_file'),
        ('forms', 'answer', 'answer_file_large'),
        ('cms', 'image', 'cms_image'),
        ('cms', 'file', 'cms_file')
$$;

-- The roles a legacy Media does not fit, the database's copy of
-- rolesWithoutLegacy (internal/media/attachment.go); a test keeps the two
-- equal. They were made after Media purpose, so no Media was ever linked in
-- them without one, and their purposes are sent by Direct upload (club
-- files also scanned), which a legacy upload never was.
CREATE OR REPLACE FUNCTION media_roles_without_legacy()
RETURNS TABLE (owner_service TEXT, role TEXT)
LANGUAGE sql
IMMUTABLE
AS $$
    VALUES
        ('core', 'event_file'),
        ('core', 'event_video')
$$;

-- Whether a Media of the purpose may play the product's role: a purpose the
-- role table names for it, or legacy (the transition rule), except in a role
-- a legacy Media does not fit.
CREATE OR REPLACE FUNCTION media_purpose_fits_role(owner_service TEXT, role TEXT, purpose TEXT)
RETURNS BOOLEAN
LANGUAGE sql
IMMUTABLE
AS $$
    SELECT CASE WHEN purpose = 'legacy' THEN NOT EXISTS (
        SELECT 1 FROM media_roles_without_legacy() strict_role
        WHERE strict_role.owner_service = media_purpose_fits_role.owner_service
          AND strict_role.role = media_purpose_fits_role.role
    ) ELSE EXISTS (
        SELECT 1 FROM media_role_purposes() fitting
        WHERE fitting.owner_service = media_purpose_fits_role.owner_service
          AND fitting.role = media_purpose_fits_role.role
          AND fitting.purpose = media_purpose_fits_role.purpose
    ) END
$$;
