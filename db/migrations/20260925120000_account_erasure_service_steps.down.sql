-- Forward-only once a service erasure step exists: its row is completion
-- proof that must be kept for at least three years, and narrowing the step
-- list back would have to delete it. Ship a forward repair instead.
LOCK TABLE account_deletion_steps IN ACCESS EXCLUSIVE MODE;

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM account_deletion_steps
        WHERE step IN ('erase_skymail', 'erase_cms', 'erase_forms') OR counts IS NOT NULL
    ) THEN
        RAISE EXCEPTION 'account erasure service steps are forward-only: a service erasure checkpoint is completion proof; ship a forward repair instead';
    END IF;
END
$$;

ALTER TABLE account_deletion_steps
    DROP CONSTRAINT IF EXISTS account_deletion_steps_counts_check,
    DROP COLUMN IF EXISTS counts,
    DROP CONSTRAINT IF EXISTS account_deletion_steps_step_check,
    ADD CONSTRAINT account_deletion_steps_step_check
        CHECK (step IN ('disable_identity', 'logout_sessions', 'anonymize_core', 'erase_profile_media', 'erase_staged_uploads', 'delete_identity'));
