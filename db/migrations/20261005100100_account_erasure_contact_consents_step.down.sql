-- Forward-only once the step has run: its checkpoint is completion proof that
-- must be kept for at least three years. Ship a forward repair instead.
LOCK TABLE account_deletion_steps IN ACCESS EXCLUSIVE MODE;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM account_deletion_steps WHERE step = 'erase_contact_consents') THEN
        RAISE EXCEPTION 'erase_contact_consents checkpoints are completion proof; ship a forward repair instead';
    END IF;
END
$$;

ALTER TABLE account_deletion_steps
    DROP CONSTRAINT IF EXISTS account_deletion_steps_step_check,
    ADD CONSTRAINT account_deletion_steps_step_check
        CHECK (step IN (
            'disable_identity', 'logout_sessions',
            'erase_skymail', 'erase_cms', 'erase_forms',
            'anonymize_core', 'erase_profile_media', 'erase_staged_uploads', 'delete_identity'
        ));
