-- Image sizes (ADR-0052, media redesign ticket 04). A raster image Media
-- records its width and height as stored, and the sizes core stored beside
-- it as objects (<key>/<size>): {"card": {"width": 400, "height": 300}}.
-- size_objects is NULL until core has made the sizes: images stored before this
-- migration wait for the variant backfill, which records {} for an image
-- that needs none or cannot be decoded.
ALTER TABLE media
    ADD COLUMN IF NOT EXISTS width INTEGER CONSTRAINT media_width_check CHECK (width > 0),
    ADD COLUMN IF NOT EXISTS height INTEGER CONSTRAINT media_height_check CHECK (height > 0),
    ADD COLUMN IF NOT EXISTS size_objects JSONB
        CONSTRAINT media_size_objects_check CHECK (size_objects IS NULL OR jsonb_typeof(size_objects) = 'object');

-- The variant backfill reads the current images still waiting for their sizes.
CREATE INDEX IF NOT EXISTS media_size_objects_pending_idx
    ON media (id)
    WHERE size_objects IS NULL AND kind = 'IMAGE' AND deleted_at IS NULL
      AND blob_purge_started_at IS NULL AND blob_purged_at IS NULL;
