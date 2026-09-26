-- Private Media (media redesign ticket 06, ADR-0052). A private Media's
-- object is in the private bucket, encrypted with its own data key; the
-- record keeps that key as the OpenBao Transit key wrapped it, the key
-- version that did, and the format. A public Media has none of them.
ALTER TABLE media
    ADD COLUMN IF NOT EXISTS visibility TEXT NOT NULL DEFAULT 'public',
    ADD COLUMN IF NOT EXISTS encryption_algorithm TEXT,
    ADD COLUMN IF NOT EXISTS wrapped_data_key TEXT,
    ADD COLUMN IF NOT EXISTS key_version INTEGER;

ALTER TABLE media DROP CONSTRAINT IF EXISTS media_visibility_check;
ALTER TABLE media ADD CONSTRAINT media_visibility_check CHECK (
    (visibility = 'public' AND encryption_algorithm IS NULL AND wrapped_data_key IS NULL AND key_version IS NULL)
    OR (visibility = 'private' AND encryption_algorithm IS NOT NULL AND wrapped_data_key IS NOT NULL
        AND key_version IS NOT NULL AND key_version >= 1)
);

-- The access log of private Media: every read link core issues (which Media,
-- for which product, the person it acted for, when, until when) and every
-- successful open of one (when, from which address). Rows are kept one year
-- (media.ReadLinkRetention); an open goes with its link.
CREATE TABLE IF NOT EXISTS media_read_links (
    id UUID PRIMARY KEY,
    media_id UUID NOT NULL REFERENCES media(id),
    product TEXT NOT NULL,
    on_behalf_of UUID NOT NULL,
    issued_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT media_read_links_expiry_check CHECK (expires_at > issued_at)
);
CREATE INDEX IF NOT EXISTS media_read_links_media_idx ON media_read_links (media_id, issued_at);
CREATE INDEX IF NOT EXISTS media_read_links_issued_idx ON media_read_links (issued_at);

CREATE TABLE IF NOT EXISTS media_read_link_opens (
    link_id UUID NOT NULL REFERENCES media_read_links(id) ON DELETE CASCADE,
    opened_at TIMESTAMPTZ NOT NULL,
    client_ip TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS media_read_link_opens_link_idx ON media_read_link_opens (link_id, opened_at);
CREATE INDEX IF NOT EXISTS media_read_link_opens_opened_idx ON media_read_link_opens (opened_at);
