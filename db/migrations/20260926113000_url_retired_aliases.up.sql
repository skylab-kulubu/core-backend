CREATE INDEX IF NOT EXISTS urls_alias_lower_idx ON urls (lower(alias));

CREATE TABLE IF NOT EXISTS url_retired_aliases (
    alias TEXT PRIMARY KEY,
    url_id UUID REFERENCES urls (id) ON DELETE SET NULL,
    retired_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS url_retired_aliases_lower_idx ON url_retired_aliases (lower(alias));
