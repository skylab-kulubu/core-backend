DROP TRIGGER IF EXISTS certificate_template_versions_sync_media_attachments ON certificate_template_versions;
DROP TRIGGER IF EXISTS certificate_templates_sync_media_attachments ON certificate_templates;
DROP TRIGGER IF EXISTS users_sync_media_attachments ON users;
DROP TRIGGER IF EXISTS event_images_sync_media_attachments ON event_images;
DROP TRIGGER IF EXISTS events_sync_media_attachments ON events;
DROP FUNCTION IF EXISTS sync_core_media_attachments();
DROP FUNCTION IF EXISTS certificate_layout_media_ids(JSONB, JSONB);

DROP TABLE IF EXISTS media_attachments;
DROP FUNCTION IF EXISTS media_attachment_status();
DROP FUNCTION IF EXISTS require_current_attached_media();

DROP INDEX IF EXISTS media_expiry_candidates_idx;

ALTER TABLE media
    DROP COLUMN IF EXISTS expires_at,
    DROP COLUMN IF EXISTS status,
    ADD COLUMN IF NOT EXISTS attached BOOLEAN NOT NULL DEFAULT false;
