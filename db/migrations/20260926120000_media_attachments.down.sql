DROP TRIGGER IF EXISTS certificate_template_versions_media_attachments_delete ON certificate_template_versions;
DROP TRIGGER IF EXISTS certificate_template_versions_media_attachments_update ON certificate_template_versions;
DROP TRIGGER IF EXISTS certificate_template_versions_media_attachments_insert ON certificate_template_versions;
DROP TRIGGER IF EXISTS certificate_templates_media_attachments_delete ON certificate_templates;
DROP TRIGGER IF EXISTS certificate_templates_media_attachments_update ON certificate_templates;
DROP TRIGGER IF EXISTS certificate_templates_media_attachments_insert ON certificate_templates;
DROP TRIGGER IF EXISTS users_media_attachments_delete ON users;
DROP TRIGGER IF EXISTS users_media_attachments_update ON users;
DROP TRIGGER IF EXISTS users_media_attachments_insert ON users;
DROP TRIGGER IF EXISTS event_images_media_attachments_delete ON event_images;
DROP TRIGGER IF EXISTS event_images_media_attachments_update ON event_images;
DROP TRIGGER IF EXISTS event_images_media_attachments_insert ON event_images;
DROP TRIGGER IF EXISTS events_media_attachments_delete ON events;
DROP TRIGGER IF EXISTS events_media_attachments_update ON events;
DROP TRIGGER IF EXISTS events_media_attachments_insert ON events;
DROP FUNCTION IF EXISTS sync_core_media_attachments();
DROP FUNCTION IF EXISTS core_media_links(JSONB, TEXT, TEXT, TEXT);
DROP FUNCTION IF EXISTS certificate_layout_media_ids(JSONB, JSONB);

DROP TABLE IF EXISTS media_attachments;
DROP FUNCTION IF EXISTS media_attachment_status();
DROP FUNCTION IF EXISTS require_current_attached_media();

DROP INDEX IF EXISTS media_expiry_idx;

ALTER TABLE media
    DROP COLUMN IF EXISTS expires_at,
    DROP COLUMN IF EXISTS status,
    ADD COLUMN IF NOT EXISTS attached BOOLEAN NOT NULL DEFAULT false;
