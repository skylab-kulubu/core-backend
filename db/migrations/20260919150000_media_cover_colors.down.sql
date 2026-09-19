DROP INDEX IF EXISTS media_cover_colors_pending_idx;

ALTER TABLE media
    DROP COLUMN IF EXISTS cover_colors_computed,
    DROP COLUMN IF EXISTS cover_colors;
