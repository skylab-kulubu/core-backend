CREATE TABLE event_days (
    id UUID PRIMARY KEY,
    event_id UUID NOT NULL REFERENCES events (id) ON DELETE CASCADE,
    name TEXT NOT NULL DEFAULT '',
    start_date TIMESTAMPTZ,
    end_date TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE tickets (
    id UUID PRIMARY KEY,
    event_id UUID NOT NULL REFERENCES events (id) ON DELETE CASCADE,
    ticket_type TEXT NOT NULL,
    owner_id UUID REFERENCES users (id) ON DELETE SET NULL,
    guest_first_name TEXT NOT NULL DEFAULT '',
    guest_last_name TEXT NOT NULL DEFAULT '',
    guest_email TEXT NOT NULL DEFAULT '',
    guest_phone_number TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX tickets_owner_event_idx ON tickets (owner_id, event_id) WHERE owner_id IS NOT NULL;
CREATE UNIQUE INDEX tickets_guest_event_idx ON tickets (guest_email, event_id) WHERE guest_email <> '';

CREATE TABLE ticket_checkins (
    id UUID PRIMARY KEY,
    ticket_id UUID NOT NULL REFERENCES tickets (id) ON DELETE CASCADE,
    event_day_id UUID NOT NULL REFERENCES event_days (id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (ticket_id, event_day_id)
);
