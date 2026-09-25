DROP INDEX IF EXISTS certificate_template_versions_asset_serving_pending_idx;

ALTER TABLE certificate_template_versions
    DROP COLUMN IF EXISTS asset_serving_policy_applied;

DROP INDEX IF EXISTS media_serving_policy_pending_idx;

ALTER TABLE media
    DROP COLUMN IF EXISTS serving_policy_applied;
