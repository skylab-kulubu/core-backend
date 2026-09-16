CREATE TABLE media (
    id UUID PRIMARY KEY,
    file_name TEXT NOT NULL DEFAULT '',
    file_type TEXT NOT NULL DEFAULT '',
    file_url TEXT NOT NULL,
    file_size BIGINT NOT NULL DEFAULT 0,
    uploaded_by UUID NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    kind TEXT NOT NULL,
    attached BOOLEAN NOT NULL DEFAULT false,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX media_uploaded_by_idx ON media (uploaded_by);
