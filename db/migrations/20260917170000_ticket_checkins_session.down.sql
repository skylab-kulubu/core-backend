DELETE FROM ticket_checkins;

DROP INDEX IF EXISTS ticket_checkins_session_id_idx;

ALTER TABLE ticket_checkins
    DROP CONSTRAINT ticket_checkins_ticket_id_session_id_key;

ALTER TABLE ticket_checkins
    DROP COLUMN session_id;

ALTER TABLE ticket_checkins
    ADD CONSTRAINT ticket_checkins_ticket_id_event_day_id_key UNIQUE (ticket_id, event_day_id);
