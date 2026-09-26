-- The stored size objects stay in the bucket; nothing reads them after this.
DROP INDEX IF EXISTS media_size_objects_pending_idx;
ALTER TABLE media
    DROP COLUMN IF EXISTS size_objects,
    DROP COLUMN IF EXISTS height,
    DROP COLUMN IF EXISTS width;
