CREATE TABLE event_door_staff (
    event_id UUID NOT NULL REFERENCES events (id) ON DELETE CASCADE,
    user_id UUID NOT NULL,
    PRIMARY KEY (event_id, user_id)
);

CREATE INDEX event_door_staff_user_id_idx ON event_door_staff (user_id);
