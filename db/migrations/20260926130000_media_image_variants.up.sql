-- Image sizes (ADR-0052, media redesign ticket 04). A raster image Media
-- records its width and height as stored, and the sizes core stored beside
-- it as objects (<key>/<size>): {"card": {"width": 400, "height": 300}}.
-- variants is NULL until core has made the sizes: images stored before this
-- migration wait for the variant backfill, which records {} for an image
-- that needs none or cannot be decoded.
ALTER TABLE media
    ADD COLUMN IF NOT EXISTS width INTEGER CONSTRAINT media_width_check CHECK (width > 0),
    ADD COLUMN IF NOT EXISTS height INTEGER CONSTRAINT media_height_check CHECK (height > 0),
    ADD COLUMN IF NOT EXISTS variants JSONB
        CONSTRAINT media_variants_check CHECK (variants IS NULL OR jsonb_typeof(variants) = 'object');

-- The variant backfill reads the current images still waiting for their sizes.
CREATE INDEX IF NOT EXISTS media_variants_pending_idx
    ON media (id)
    WHERE variants IS NULL AND kind = 'IMAGE' AND deleted_at IS NULL
      AND blob_purge_started_at IS NULL AND blob_purged_at IS NULL;
