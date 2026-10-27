-- Puts back the checks without the team page purpose and the web channel.
-- They are added NOT VALID so that the grants already written stay: they
-- are consent records (proof).
ALTER TABLE contact_consents DROP CONSTRAINT IF EXISTS contact_consents_purpose_check;
ALTER TABLE contact_consents ADD CONSTRAINT contact_consents_purpose_check
    CHECK (purpose IN ('event_invitations', 'recruitment_pool', 'promotional')) NOT VALID;

ALTER TABLE contact_consents DROP CONSTRAINT IF EXISTS contact_consents_channel_check;
ALTER TABLE contact_consents ADD CONSTRAINT contact_consents_channel_check
    CHECK (channel IN ('email')) NOT VALID;
