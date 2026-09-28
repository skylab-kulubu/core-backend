-- Direct upload (media redesign ticket 11, ADR-0052). The browser writes a
-- Direct upload's parts straight to storage, under pending/<id>, and core
-- completes it later. The pending object is registered in
-- media_upload_staging like every object write core causes: its staging row
-- (the key, the uploader, cleanup_after = the upload's expiry) is what the
-- staging sweeper and account erasure's erase_staged_uploads delete by, and
-- deleting a pending key also aborts the multipart upload open at it
-- (media.R2.Delete). This table holds only what the upload needs besides,
-- and goes with that staging row: the file name never outlives it.
--
-- id is also the id the Media gets once the upload completes, so a
-- completion retried after its answer was lost finds the Media.
CREATE TABLE IF NOT EXISTS media_direct_uploads (
    id UUID PRIMARY KEY,
    object_key TEXT NOT NULL,
    purpose TEXT NOT NULL,
    file_name TEXT NOT NULL,
    declared_size BIGINT NOT NULL,
    max_bytes BIGINT NOT NULL,
    allowed_types TEXT[] NOT NULL,
    part_size BIGINT NOT NULL,
    -- Storage's multipart upload id: empty until storage opened it.
    multipart_upload_id TEXT NOT NULL DEFAULT '',
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

ALTER TABLE media_direct_uploads
    DROP CONSTRAINT IF EXISTS media_direct_uploads_object_key_key,
    ADD CONSTRAINT media_direct_uploads_object_key_key UNIQUE (object_key),
    DROP CONSTRAINT IF EXISTS media_direct_uploads_object_key_fkey,
    ADD CONSTRAINT media_direct_uploads_object_key_fkey
        FOREIGN KEY (object_key) REFERENCES media_upload_staging (object_key) ON DELETE CASCADE,
    DROP CONSTRAINT IF EXISTS media_direct_uploads_sizes_check,
    ADD CONSTRAINT media_direct_uploads_sizes_check
        CHECK (declared_size > 0 AND declared_size <= max_bytes AND part_size > 0);
