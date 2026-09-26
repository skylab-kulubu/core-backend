-- A Media attachment's owner id is the owning product's own id for its record
-- (media redesign ticket 03). Core's records and a Skyforms response have
-- UUIDs, but a CMS page is its site's client id and its slug
-- (clientId:slug); only CMS blocks and collection items have Guids. The id
-- becomes text; core's own links keep their records' UUIDs, written as text.
ALTER TABLE media_attachments
    ALTER COLUMN owner_id TYPE TEXT USING owner_id::TEXT;

ALTER TABLE media_attachments
    DROP CONSTRAINT IF EXISTS media_attachments_owner_id_check;
ALTER TABLE media_attachments
    ADD CONSTRAINT media_attachments_owner_id_check CHECK (owner_id <> '' AND length(owner_id) <= 200);

-- Core's own links compare and write their owners' UUIDs as text.
CREATE OR REPLACE FUNCTION sync_core_media_attachments()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    owner_kind CONSTANT TEXT := TG_ARGV[0];
    link_role CONSTANT TEXT := TG_ARGV[1];
    before_links JSONB := '[]';
    after_links JSONB := '[]';
BEGIN
    IF TG_OP IN ('UPDATE', 'DELETE') THEN
        SELECT COALESCE(jsonb_agg(to_jsonb(link)), '[]') INTO before_links
        FROM old_owners owner
        CROSS JOIN LATERAL core_media_links(to_jsonb(owner), TG_ARGV[2], TG_ARGV[3], TG_ARGV[4]) link;
    END IF;
    IF TG_OP IN ('INSERT', 'UPDATE') THEN
        SELECT COALESCE(jsonb_agg(to_jsonb(link)), '[]') INTO after_links
        FROM new_owners owner
        CROSS JOIN LATERAL core_media_links(to_jsonb(owner), TG_ARGV[2], TG_ARGV[3], TG_ARGV[4]) link;
    END IF;

    DELETE FROM media_attachments a
    USING (
        SELECT * FROM jsonb_to_recordset(before_links) AS link (owner_id UUID, media_id UUID)
        EXCEPT
        SELECT * FROM jsonb_to_recordset(after_links) AS link (owner_id UUID, media_id UUID)
    ) gone
    WHERE a.owner_service = 'core' AND a.owner_type = owner_kind AND a.role = link_role
      AND a.owner_id = gone.owner_id::TEXT AND a.media_id = gone.media_id;
    INSERT INTO media_attachments (media_id, owner_service, owner_type, owner_id, role)
    SELECT added.media_id, 'core', owner_kind, added.owner_id::TEXT, link_role
    FROM (
        SELECT * FROM jsonb_to_recordset(after_links) AS link (owner_id UUID, media_id UUID)
        EXCEPT
        SELECT * FROM jsonb_to_recordset(before_links) AS link (owner_id UUID, media_id UUID)
    ) added
    ON CONFLICT ON CONSTRAINT media_attachments_link_key DO NOTHING;
    RETURN NULL;
END;
$$;
