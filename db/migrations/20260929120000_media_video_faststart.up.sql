-- Video faststart (media redesign ticket 13). A video's MP4 whose moov box
-- comes after its media data plays only once it has downloaded whole. The
-- faststart worker rewrites it with its moov in front (no re-encode) into a
-- new object beside it, videos/<uuid>.fs.mp4, and points the Media there;
-- the original is deleted an hour later, once the copy is checked there.
-- Video Media stored before this wait for it (NULL) like new ones.
ALTER TABLE media
    -- Where the rewrite is: NULL while the video waits for it; moved once
    -- the Media points at its faststart copy and its original waits out
    -- its hour; then done (the original is gone), or not_needed (its moov
    -- came first already), or failed (a file the rewrite cannot move
    -- safely, one it kept failing on, or one it panicked on; it is served
    -- as it is, and plays once downloaded).
    ADD COLUMN IF NOT EXISTS video_faststart TEXT,
    -- The worker's backoff: steps failed in a row, and when to try again.
    -- A moved Media waits here for its hour.
    ADD COLUMN IF NOT EXISTS video_faststart_attempts INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS video_faststart_retry_at TIMESTAMPTZ,
    -- A worker's claim on the Media, taken in a short transaction before
    -- any storage work and let go when its step ends: only one worker (of
    -- the core replicas a rolling deploy runs side by side) rewrites a
    -- video at a time. The archive and expiry purges wait for a live lease.
    ADD COLUMN IF NOT EXISTS video_faststart_claim_id UUID,
    ADD COLUMN IF NOT EXISTS video_faststart_claimed_until TIMESTAMPTZ;

ALTER TABLE media
    DROP CONSTRAINT IF EXISTS media_video_faststart_check,
    ADD CONSTRAINT media_video_faststart_check CHECK (
        (video_faststart IS NULL OR video_faststart IN ('moved', 'done', 'not_needed', 'failed'))
        AND video_faststart_attempts >= 0
        AND (video_faststart_claim_id IS NULL) = (video_faststart_claimed_until IS NULL)
    );

-- The worker walks the videos waiting for their rewrite, and the moved
-- ones waiting for their original to go.
CREATE INDEX IF NOT EXISTS media_video_faststart_due_idx
    ON media (id)
    WHERE purpose = 'video' AND (video_faststart IS NULL OR video_faststart = 'moved') AND blob_purged_at IS NULL;
