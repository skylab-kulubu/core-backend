-- Account erasure deletes the person's contact consents (ADR-0062,
-- docs/contact-consents.md) in a core-local step of its own,
-- erase_contact_consents, right after logout_sessions and before the service
-- steps, so no invitation goes out while a service holds the saga. The
-- consent key turns the person's addresses into the proof keys of their
-- ended grants. The order of the list below is not the saga's order.
ALTER TABLE account_deletion_steps
    DROP CONSTRAINT IF EXISTS account_deletion_steps_step_check,
    ADD CONSTRAINT account_deletion_steps_step_check
        CHECK (step IN (
            'disable_identity', 'logout_sessions',
            'erase_skymail', 'erase_cms', 'erase_forms',
            'erase_contact_consents',
            'anonymize_core', 'erase_profile_media', 'erase_staged_uploads', 'delete_identity'
        ));
