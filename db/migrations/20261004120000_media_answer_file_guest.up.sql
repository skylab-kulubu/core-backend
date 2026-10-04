-- A guest Answer file (answer_file_guest): an Answer file sent to a Skyforms
-- form that takes answers without sign-in. Skyforms' service account
-- uploads it; no person does, and the service account has no users row.
--
-- Its single-step upload is staged like any other before the object write,
-- for no one: media_upload_staging.subject_id may be NULL. The account
-- reference guard (require_active_account_reference) passes NULL, the
-- sweeper reads rows by cleanup_after alone, and account erasure's fence
-- (subject_id = $1) never matches a staged upload of no one. Its Media has
-- no uploader (media.uploaded_by NULL), as a video's frame has none.
--
-- A rerun of 20260920010000 (a database that lost its record of it) sets
-- the column NOT NULL again; it fails while any staged upload of no one is
-- left, and otherwise this migration's fingerprint finds the column NOT
-- NULL and it runs again.
ALTER TABLE media_upload_staging ALTER COLUMN subject_id DROP NOT NULL;

-- 20260929160000's copy of rolePurposes (internal/media/attachment.go),
-- with the guest Answer file, which Skyforms links as an answer. A test
-- keeps it equal to rolePurposes, both ways.
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
        ('core', 'event_video_poster', 'event_cover'),
        ('core', 'event_video_poster', 'event_gallery'),
        ('core', 'event_video_frame', 'video_frame'),
        ('forms', 'answer', 'answer_file'),
        ('forms', 'answer', 'answer_file_large'),
        ('forms', 'answer', 'answer_file_guest'),
        ('cms', 'image', 'cms_image'),
        ('cms', 'file', 'cms_file')
$$;
