-- Dropping the wrapped data keys would make every private Media unreadable
-- for good, so this refuses while any exists.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM media WHERE visibility = 'private') THEN
        RAISE EXCEPTION 'private Media exist: dropping their wrapped data keys would lose them';
    END IF;
END
$$;

DROP TABLE IF EXISTS media_read_link_opens;
DROP TABLE IF EXISTS media_read_links;
ALTER TABLE media DROP CONSTRAINT IF EXISTS media_visibility_check;
ALTER TABLE media
    DROP COLUMN IF EXISTS key_version,
    DROP COLUMN IF EXISTS wrapped_data_key,
    DROP COLUMN IF EXISTS encryption_algorithm,
    DROP COLUMN IF EXISTS visibility;
