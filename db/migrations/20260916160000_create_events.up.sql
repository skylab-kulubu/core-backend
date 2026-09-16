CREATE TABLE events (
    id UUID PRIMARY KEY,
    name TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    location TEXT NOT NULL,
    owner_team TEXT NOT NULL,
    form_url TEXT NOT NULL DEFAULT '',
    capacity INTEGER NOT NULL DEFAULT 0,
    start_date TIMESTAMPTZ,
    end_date TIMESTAMPTZ,
    linkedin TEXT NOT NULL DEFAULT '',
    active BOOLEAN NOT NULL DEFAULT false,
    ranked BOOLEAN NOT NULL DEFAULT false,
    prize_info TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX events_owner_team_idx ON events (owner_team);
