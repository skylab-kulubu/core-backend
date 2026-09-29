-- The address lookup (media redesign ticket 28): a product asks which Media
-- the CDN addresses its content keeps name. An address names a Media by
-- its key; a video moved to its faststart copy
-- (videos/<uuid>.fs.<claim>.mp4) is named by its original's key too
-- (videos/<uuid>.mp4), which content may still hold. media_lookup_key is
-- the key a Media is looked up by: its own, or its original's for a
-- faststart copy (lookupKeyOf in internal/media/lookup.go; a test keeps
-- the two equal). The lookup reads a whole batch in one query over the
-- index below.
--
-- The index is dropped first: it holds what the function answered when it
-- was built, so a function this migration puts back needs it built again.
DROP INDEX IF EXISTS media_lookup_key_idx;

CREATE OR REPLACE FUNCTION media_lookup_key(key TEXT)
RETURNS TEXT
LANGUAGE sql
IMMUTABLE
STRICT
PARALLEL SAFE
AS $$
    SELECT regexp_replace(key, '^(videos/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})\.fs\.[0-9a-f]{32}\.mp4$', '\1.mp4')
$$;

CREATE INDEX media_lookup_key_idx ON media (media_lookup_key(file_url));
