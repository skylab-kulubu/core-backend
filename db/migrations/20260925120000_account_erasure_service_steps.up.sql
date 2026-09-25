-- Service erasure steps (ADR-0051). Core sends SkyMail, CMS and Forms one
-- Erasure command each; every confirmation is a checkpoint row that also keeps
-- the service's counts. Counts are the completion proof kept for at least
-- three years (KVKK deletion regulation art. 7(3)): snake_case keys chosen by
-- the service and non-negative integers, never an address or a name.
ALTER TABLE account_deletion_steps
    ADD COLUMN IF NOT EXISTS counts JSONB;

ALTER TABLE account_deletion_steps
    DROP CONSTRAINT IF EXISTS account_deletion_steps_step_check,
    ADD CONSTRAINT account_deletion_steps_step_check
        CHECK (step IN (
            'disable_identity', 'logout_sessions',
            'erase_skymail', 'erase_cms', 'erase_forms',
            'anonymize_core', 'erase_profile_media', 'erase_staged_uploads', 'delete_identity'
        )),
    DROP CONSTRAINT IF EXISTS account_deletion_steps_counts_check,
    ADD CONSTRAINT account_deletion_steps_counts_check
        CHECK (counts IS NULL OR jsonb_typeof(counts) = 'object');

COMMENT ON COLUMN account_deletion_steps.counts IS
    'Service erasure proof: the counts object from the service''s 200 answer. No e-mail, name or subject.';
