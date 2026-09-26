-- The uploads of an account erasure (media redesign ticket 07, ADR-0052).
-- anonymize_core records here, by id only, every upload of the person but
-- their current profile picture; erase_profile_media then purges the
-- person's own and strips their file name from the club content it keeps.
-- No file name, object key or purpose is copied: the Media record has them,
-- and the person's uploader link is cleared in the same transaction, so this
-- is the only way back to them.
--
-- A row goes in the transaction that purges its Media, or once the club
-- content is kept, so the table holds a request's rows only until
-- erase_profile_media is done. A row still there when the request is to
-- complete is an upload that was never handled: completion is refused (the
-- trigger below) rather than losing the only way back to it, so the
-- completion proof, kept for at least three years, never carries this list
-- and no upload is left behind unseen.
CREATE TABLE IF NOT EXISTS account_deletion_media (
    request_id UUID NOT NULL,
    media_id UUID NOT NULL
);

-- media_id cascades nowhere: Media rows are never deleted, and a Media that
-- was would first have to be let go by the erasure that still holds it.
ALTER TABLE account_deletion_media
    DROP CONSTRAINT IF EXISTS account_deletion_media_pkey,
    ADD CONSTRAINT account_deletion_media_pkey PRIMARY KEY (request_id, media_id),
    DROP CONSTRAINT IF EXISTS account_deletion_media_request_id_fkey,
    ADD CONSTRAINT account_deletion_media_request_id_fkey
        FOREIGN KEY (request_id) REFERENCES account_deletion_requests (id) ON DELETE CASCADE,
    DROP CONSTRAINT IF EXISTS account_deletion_media_media_id_fkey,
    ADD CONSTRAINT account_deletion_media_media_id_fkey
        FOREIGN KEY (media_id) REFERENCES media (id);

-- The purge finds and removes a Media's row by the Media id.
DROP INDEX IF EXISTS account_deletion_media_media_idx;
CREATE INDEX account_deletion_media_media_idx ON account_deletion_media (media_id);

-- No id in the message: the worker logs it.
CREATE OR REPLACE FUNCTION require_account_deletion_media_erased() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM account_deletion_media WHERE request_id = NEW.id) THEN
        RAISE EXCEPTION 'account erasure has recorded uploads left to erase' USING ERRCODE = '55000';
    END IF;
    RETURN NEW;
END
$$;

DROP TRIGGER IF EXISTS account_deletion_requests_require_media_erased ON account_deletion_requests;
CREATE TRIGGER account_deletion_requests_require_media_erased
    BEFORE UPDATE OF status ON account_deletion_requests
    FOR EACH ROW WHEN (NEW.status = 'completed')
    EXECUTE FUNCTION require_account_deletion_media_erased();
