DROP INDEX IF EXISTS media_serving_policy_pending_idx;

ALTER TABLE media
    DROP COLUMN IF EXISTS serving_policy_applied;
