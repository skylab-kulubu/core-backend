ALTER TABLE events
    ADD COLUMN cover_image_id UUID REFERENCES media (id) ON DELETE SET NULL;

CREATE INDEX events_cover_image_id_idx ON events (cover_image_id);
