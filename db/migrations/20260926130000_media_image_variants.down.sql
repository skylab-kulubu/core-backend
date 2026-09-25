-- The stored size objects stay in the bucket; nothing reads them after this.
DROP INDEX IF EXISTS media_variants_pending_idx;
ALTER TABLE media
    DROP COLUMN IF EXISTS variants,
    DROP COLUMN IF EXISTS height,
    DROP COLUMN IF EXISTS width;
