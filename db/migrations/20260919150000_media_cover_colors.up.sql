ALTER TABLE media
    ADD COLUMN IF NOT EXISTS cover_colors TEXT[] NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS cover_colors_computed BOOLEAN NOT NULL DEFAULT false;

CREATE INDEX IF NOT EXISTS media_cover_colors_pending_idx
    ON media (created_at)
    WHERE kind = 'image' AND cover_colors_computed = false;
