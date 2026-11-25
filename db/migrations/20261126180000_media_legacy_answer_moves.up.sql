SET LOCAL lock_timeout = '5s';

-- Legacy Answer files (docs/media-lifecycle.md, "Legacy Answer files"):
-- Skyforms Answer files uploaded before Media purpose are legacy Media,
-- public and unscanned. `core-backend media-legacy-answers` moves each one
-- into the private bucket, encrypted, as an answer_file waiting for its
-- malware scan, linked to its Skyforms responses. This table is the move's
-- record, one row per Media: the public key it had (an id, no name), when it
-- was moved (its scan deadline runs from then, not from its upload), and
-- when its public object was deleted (NULL until then; a later run deletes
-- it). A Media row is never deleted; the cascade only keeps a test's
-- cleanup simple.
CREATE TABLE IF NOT EXISTS media_legacy_answer_moves (
    media_id UUID PRIMARY KEY REFERENCES media(id) ON DELETE CASCADE,
    public_key TEXT NOT NULL CHECK (public_key <> ''),
    moved_at TIMESTAMPTZ NOT NULL,
    public_deleted_at TIMESTAMPTZ,
    CONSTRAINT media_legacy_answer_moves_order_check CHECK (public_deleted_at IS NULL OR public_deleted_at >= moved_at)
);
