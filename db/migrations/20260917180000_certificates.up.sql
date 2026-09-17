ALTER TABLE events
    ADD COLUMN attendance_rule TEXT NOT NULL DEFAULT 'none',
    ADD COLUMN attendance_ratio DOUBLE PRECISION;

ALTER TABLE sessions
    ADD COLUMN cancelled BOOLEAN NOT NULL DEFAULT false;

CREATE TABLE certificates (
    id UUID PRIMARY KEY,
    event_id UUID NOT NULL REFERENCES events (id) ON DELETE CASCADE,
    ticket_id UUID NOT NULL REFERENCES tickets (id) ON DELETE CASCADE,
    owner_id UUID REFERENCES users (id) ON DELETE SET NULL,
    serial TEXT NOT NULL UNIQUE,
    recipient_name TEXT NOT NULL,
    recipient_email TEXT NOT NULL,
    event_name TEXT NOT NULL,
    owner_team TEXT NOT NULL,
    verify_url TEXT NOT NULL,
    pdf BYTEA NOT NULL DEFAULT ''::bytea,
    revoked_at TIMESTAMPTZ,
    issued_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX certificates_event_ticket_active_idx
    ON certificates (event_id, ticket_id)
    WHERE revoked_at IS NULL;

CREATE INDEX certificates_owner_id_idx ON certificates (owner_id);
CREATE INDEX certificates_event_id_idx ON certificates (event_id);
