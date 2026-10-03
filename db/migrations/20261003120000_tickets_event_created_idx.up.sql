-- The dashboard summary (GET /v1/dashboard/summary) counts the Tickets of
-- the Events a caller covers and those of the last 30 days; the applicant
-- list (GET /v1/events/{id}/tickets) reads one Event's Tickets. No index led
-- with event_id, so each of them scanned every Ticket. The index is dropped
-- first so that one of another shape under this name is replaced.
DROP INDEX IF EXISTS tickets_event_created_idx;
CREATE INDEX tickets_event_created_idx ON tickets (event_id, created_at);
