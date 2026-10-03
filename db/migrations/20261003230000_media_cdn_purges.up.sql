-- CDN cache purge (media redesign ticket 29): the addresses of public
-- objects core deleted, or gave new metadata, wait here until Cloudflare has
-- purged its copy of them (internal/media/cdn_purge.go). One row per
-- address; queuing it again makes it due at once. A pass leases the rows it
-- takes by moving next_attempt_at past the lease, and a failed purge moves
-- it past its backoff. Rows older than the CDN's edge cache keeps anything
-- are dropped. The address is the CDN base and the object key: ids, no
-- names.
CREATE TABLE IF NOT EXISTS media_cdn_purges (
    url             TEXT        PRIMARY KEY,
    queued_at       TIMESTAMPTZ NOT NULL,
    next_attempt_at TIMESTAMPTZ NOT NULL,
    attempts        INTEGER     NOT NULL DEFAULT 0 CHECK (attempts >= 0)
);

CREATE INDEX IF NOT EXISTS media_cdn_purges_due_idx ON media_cdn_purges (next_attempt_at);
