-- Puts back the checks without the promotional purpose and the signup
-- source. They are added NOT VALID so that the grants already written stay:
-- they are consent records (proof).
ALTER TABLE contact_consents DROP CONSTRAINT IF EXISTS contact_consents_purpose_check;
ALTER TABLE contact_consents ADD CONSTRAINT contact_consents_purpose_check
    CHECK (purpose IN ('event_invitations', 'recruitment_pool')) NOT VALID;

ALTER TABLE contact_consents DROP CONSTRAINT IF EXISTS contact_consents_source_check;
ALTER TABLE contact_consents ADD CONSTRAINT contact_consents_source_check
    CHECK (source IN ('guest_apply', 'forms', 'place', 'guessr', 'self')) NOT VALID;
