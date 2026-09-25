-- True once the object's R2 metadata is known to follow the serving policy
-- (internal/media/serving.go): set by every upload, and by the serving policy
-- backfill for media stored before it.
ALTER TABLE media
    ADD COLUMN IF NOT EXISTS serving_policy_applied BOOLEAN NOT NULL DEFAULT false;

CREATE INDEX IF NOT EXISTS media_serving_policy_pending_idx
    ON media (id)
    WHERE serving_policy_applied = false;

-- The same for the copies a published certificate template keeps of its
-- layout Media under certificate-template-assets/.
ALTER TABLE certificate_template_versions
    ADD COLUMN IF NOT EXISTS asset_serving_policy_applied BOOLEAN NOT NULL DEFAULT false;

CREATE INDEX IF NOT EXISTS certificate_template_versions_asset_serving_pending_idx
    ON certificate_template_versions (id)
    WHERE asset_serving_policy_applied = false;
