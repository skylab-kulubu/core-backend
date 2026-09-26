-- A row is a personal Media an account erasure still has to purge, and
-- nothing else leads back to it (its uploader is already cleared), so this
-- refuses while any is left. It builds on the account lifecycle schema
-- (20260920010000): roll this back before that migration's down.
LOCK TABLE account_deletion_media IN ACCESS EXCLUSIVE MODE;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM account_deletion_media) THEN
        RAISE EXCEPTION 'account erasure has personal Media left to purge: dropping their record would keep them; let the erasure finish first';
    END IF;
END
$$;

DROP TRIGGER IF EXISTS account_deletion_requests_forget_media ON account_deletion_requests;
DROP FUNCTION IF EXISTS forget_account_deletion_media();
DROP TABLE IF EXISTS account_deletion_media;
