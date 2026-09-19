CREATE TABLE url_hits (
    id UUID PRIMARY KEY,
    url_id UUID NOT NULL REFERENCES urls (id) ON DELETE CASCADE,
    alias TEXT NOT NULL,
    at TIMESTAMPTZ NOT NULL,
    ip TEXT NOT NULL DEFAULT '',
    user_agent TEXT NOT NULL DEFAULT '',
    referer TEXT NOT NULL DEFAULT '',
    user_id UUID
);

CREATE INDEX url_hits_url_id_at_idx ON url_hits (url_id, at DESC);
