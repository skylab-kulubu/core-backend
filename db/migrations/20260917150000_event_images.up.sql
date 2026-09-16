CREATE TABLE event_images (
    event_id UUID NOT NULL REFERENCES events (id) ON DELETE CASCADE,
    media_id UUID NOT NULL REFERENCES media (id) ON DELETE CASCADE,
    PRIMARY KEY (event_id, media_id)
);

CREATE INDEX event_images_media_id_idx ON event_images (media_id);
