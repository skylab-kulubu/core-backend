CREATE TABLE seasons (
    id UUID PRIMARY KEY,
    name TEXT NOT NULL,
    start_date TIMESTAMPTZ,
    end_date TIMESTAMPTZ,
    active BOOLEAN NOT NULL DEFAULT false,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

ALTER TABLE events ADD COLUMN season_id UUID REFERENCES seasons (id) ON DELETE SET NULL;
CREATE INDEX events_season_id_idx ON events (season_id);

CREATE TABLE sessions (
    id UUID PRIMARY KEY,
    event_day_id UUID NOT NULL REFERENCES event_days (id) ON DELETE CASCADE,
    title TEXT NOT NULL,
    speaker_name TEXT NOT NULL DEFAULT '',
    speaker_linkedin TEXT NOT NULL DEFAULT '',
    description TEXT NOT NULL DEFAULT '',
    start_time TIMESTAMPTZ,
    end_time TIMESTAMPTZ,
    order_index INTEGER NOT NULL DEFAULT 0,
    session_type TEXT NOT NULL
);

CREATE INDEX sessions_event_day_id_idx ON sessions (event_day_id);
