DELETE FROM ticket_checkins;

ALTER TABLE ticket_checkins
    ADD COLUMN session_id UUID NOT NULL REFERENCES sessions (id) ON DELETE CASCADE;

ALTER TABLE ticket_checkins
    DROP CONSTRAINT IF EXISTS ticket_checkins_ticket_id_event_day_id_key;

ALTER TABLE ticket_checkins
    ADD CONSTRAINT ticket_checkins_ticket_id_session_id_key UNIQUE (ticket_id, session_id);

CREATE INDEX ticket_checkins_session_id_idx ON ticket_checkins (session_id);
