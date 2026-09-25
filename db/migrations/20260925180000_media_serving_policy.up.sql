-- True once the object's R2 metadata is known to follow the serving policy
-- (internal/media/serving.go): set by every upload, and by the serving policy
-- backfill for media stored before it.
ALTER TABLE media
    ADD COLUMN IF NOT EXISTS serving_policy_applied BOOLEAN NOT NULL DEFAULT false;

CREATE INDEX IF NOT EXISTS media_serving_policy_pending_idx
    ON media (id)
    WHERE serving_policy_applied = false;
