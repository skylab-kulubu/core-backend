SET LOCAL lock_timeout = '5s';

-- The editor's "tell the Ticket holders?" for a published Event's new time
-- or place (docs/event-updated-notification.md#the-editors-choice). The
-- panel asks it when a save changes the start, the end or the location; the
-- update body carries the answer (notifyTicketHolders), and a body without
-- it tells them, as before the question.
--
-- told_fields: which of start, end and location the saves that said yes
-- changed. The worker announces only those; a change saved with no is not
-- announced (the mail still shows the Event as it is when sent). NULL is a
-- row the release before this one queued: all three, as it did.
ALTER TABLE event_update_notifications ADD COLUMN IF NOT EXISTS told_fields TEXT[];

-- The record of each answer: who saved which change of a published Event
-- that was not over, whether they said to tell the holders (NULL: the body
-- did not say), and whether a notice was queued (false while
-- EVENT_UPDATED_NOTIFICATION is off). Ids and field names only. An account
-- deleted outright empties decided_by; an Event deleted takes its rows.
CREATE TABLE IF NOT EXISTS event_change_notice_choices (
    id UUID PRIMARY KEY,
    event_id UUID NOT NULL REFERENCES events (id) ON DELETE CASCADE,
    decided_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    decided_by UUID REFERENCES users (id) ON DELETE SET NULL,
    fields TEXT[] NOT NULL CHECK (cardinality(fields) > 0),
    notify BOOLEAN,
    queued BOOLEAN NOT NULL
);

CREATE INDEX IF NOT EXISTS event_change_notice_choices_event_idx ON event_change_notice_choices (event_id, decided_at);
CREATE INDEX IF NOT EXISTS event_change_notice_choices_decided_by_idx ON event_change_notice_choices (decided_by);
