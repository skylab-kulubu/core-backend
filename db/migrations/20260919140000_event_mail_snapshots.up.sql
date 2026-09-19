CREATE TABLE IF NOT EXISTS event_mail_snapshots (
    mail_list_id UUID PRIMARY KEY,
    event_id UUID NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS event_mail_snapshots_expires_at_idx
    ON event_mail_snapshots (expires_at);
