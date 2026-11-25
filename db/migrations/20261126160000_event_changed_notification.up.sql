SET LOCAL lock_timeout = '5s';

-- An Event's other details told to its Ticket holders when the editor says
-- so (event.changed, docs/event-updated-notification.md#other-details).
-- before_details: for each detail field the saves that said yes changed
-- (name, description, attendeeNote, capacity, links), the SHA-256 of its
-- value before the first of them, so the worker can tell a change undone
-- within the window. Digests only, never the text. Empty for a row of time
-- and place alone, and for the rows of the release before this one.
ALTER TABLE event_update_notifications
    ADD COLUMN IF NOT EXISTS before_details JSONB NOT NULL DEFAULT '{}'::jsonb;
