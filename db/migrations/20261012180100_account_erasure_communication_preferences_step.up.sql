-- Account erasure deletes a person's communication preferences
-- (docs/communication-preferences.md) in a core-local step of its own,
-- erase_communication_preferences, right after erase_skypass_wallet and before the
-- service steps. The order of the list below is not the saga's order.
ALTER TABLE account_deletion_steps
    DROP CONSTRAINT IF EXISTS account_deletion_steps_step_check,
    ADD CONSTRAINT account_deletion_steps_step_check
        CHECK (step IN (
            'disable_identity', 'logout_sessions',
            'erase_skymail', 'erase_cms', 'erase_forms',
            'erase_contact_consents', 'erase_skypass_wallet', 'erase_communication_preferences',
            'anonymize_core', 'erase_profile_media', 'erase_staged_uploads', 'delete_identity'
        ));
