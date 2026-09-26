-- The personal Media of an account erasure (media redesign ticket 07,
-- ADR-0052). anonymize_core records here, by id only, the uploads of the
-- person that erase_profile_media purges at once: every upload of a personal
-- purpose and every legacy upload nothing uses (decision E1). No file name,
-- object key or purpose is copied: the Media record has them, and the
-- person's uploader link is cleared in the same transaction, so this is the
-- only way back to them.
--
-- A row goes in the transaction that purges its Media. Whatever is left when
-- the request completes goes with the completion (the trigger below), so the
-- completion proof, which is kept for at least three years, never carries
-- this list.
CREATE TABLE IF NOT EXISTS account_deletion_media (
    request_id UUID NOT NULL,
    media_id UUID NOT NULL
);

ALTER TABLE account_deletion_media
    DROP CONSTRAINT IF EXISTS account_deletion_media_pkey,
    ADD CONSTRAINT account_deletion_media_pkey PRIMARY KEY (request_id, media_id),
    DROP CONSTRAINT IF EXISTS account_deletion_media_request_id_fkey,
    ADD CONSTRAINT account_deletion_media_request_id_fkey
        FOREIGN KEY (request_id) REFERENCES account_deletion_requests (id) ON DELETE CASCADE;

-- The purge finds and removes a Media's row by the Media id.
DROP INDEX IF EXISTS account_deletion_media_media_idx;
CREATE INDEX account_deletion_media_media_idx ON account_deletion_media (media_id);

CREATE OR REPLACE FUNCTION forget_account_deletion_media() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    DELETE FROM account_deletion_media WHERE request_id = NEW.id;
    RETURN NULL;
END
$$;

DROP TRIGGER IF EXISTS account_deletion_requests_forget_media ON account_deletion_requests;
CREATE TRIGGER account_deletion_requests_forget_media
    AFTER UPDATE OF status ON account_deletion_requests
    FOR EACH ROW WHEN (NEW.status = 'completed')
    EXECUTE FUNCTION forget_account_deletion_media();
