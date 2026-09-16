CREATE TABLE competitors (
    id UUID PRIMARY KEY,
    user_id UUID NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    event_id UUID NOT NULL REFERENCES events (id) ON DELETE CASCADE,
    score DOUBLE PRECISION,
    is_winner BOOLEAN NOT NULL DEFAULT false,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (user_id, event_id)
);

CREATE INDEX competitors_event_id_idx ON competitors (event_id);
CREATE INDEX competitors_user_id_idx ON competitors (user_id);
