DROP INDEX IF EXISTS events_cover_image_id_idx;
ALTER TABLE events DROP COLUMN IF EXISTS cover_image_id;
