DROP TRIGGER IF EXISTS certificate_template_versions_require_current_media ON certificate_template_versions;
DROP TRIGGER IF EXISTS certificate_templates_require_current_media ON certificate_templates;
DROP TRIGGER IF EXISTS users_require_current_profile_media ON users;
DROP TRIGGER IF EXISTS event_images_require_current_media ON event_images;
DROP TRIGGER IF EXISTS events_require_current_cover_media ON events;
DROP FUNCTION IF EXISTS require_current_certificate_layout_media();
DROP FUNCTION IF EXISTS require_current_media_reference();

DROP INDEX IF EXISTS media_blob_purge_candidates_idx;

ALTER TABLE media
    DROP COLUMN IF EXISTS blob_purge_checked_at,
    DROP COLUMN IF EXISTS blob_purged_at,
    DROP COLUMN IF EXISTS blob_purge_started_at;
