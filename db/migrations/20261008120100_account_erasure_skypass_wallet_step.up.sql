-- Account erasure withdraws the person's SkyPass passes from Google Wallet
-- (docs/skypass-google-wallet.md) in a core-local step of its own,
-- erase_skypass_wallet, right after erase_contact_consents and before the
-- service steps: the pass stops opening the door and Google's copy loses the
-- person's name and skyNumber while a service may still hold the saga. The
-- order of the list below is not the saga's order.
ALTER TABLE account_deletion_steps
    DROP CONSTRAINT IF EXISTS account_deletion_steps_step_check,
    ADD CONSTRAINT account_deletion_steps_step_check
        CHECK (step IN (
            'disable_identity', 'logout_sessions',
            'erase_skymail', 'erase_cms', 'erase_forms',
            'erase_contact_consents', 'erase_skypass_wallet',
            'anonymize_core', 'erase_profile_media', 'erase_staged_uploads', 'delete_identity'
        ));
