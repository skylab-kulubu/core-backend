CREATE TABLE urls (
    id UUID PRIMARY KEY,
    alias TEXT NOT NULL UNIQUE,
    url TEXT NOT NULL,
    click_count INTEGER NOT NULL DEFAULT 0,
    created_by UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX urls_created_by_idx ON urls (created_by);
