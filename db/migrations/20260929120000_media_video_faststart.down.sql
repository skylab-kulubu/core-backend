-- A Media moved to its faststart copy keeps pointing at it: file_url is the
-- copy's key, served as the original was.
DROP INDEX IF EXISTS media_video_faststart_due_idx;

ALTER TABLE media
    DROP CONSTRAINT IF EXISTS media_video_faststart_check,
    DROP COLUMN IF EXISTS video_faststart_claimed_until,
    DROP COLUMN IF EXISTS video_faststart_claim_id,
    DROP COLUMN IF EXISTS video_faststart_retry_at,
    DROP COLUMN IF EXISTS video_faststart_attempts,
    DROP COLUMN IF EXISTS video_faststart;
