-- The promotional consent (docs/contact-consents.md): one grant per person
-- for mails whose main purpose is promoting a product or a service. It is
-- given in Account Center's communication preferences (source self), on the
-- first Account Center screen after the account was opened (source signup)
-- and in SkyForms forms (source forms).
ALTER TABLE contact_consents DROP CONSTRAINT IF EXISTS contact_consents_purpose_check;
ALTER TABLE contact_consents ADD CONSTRAINT contact_consents_purpose_check
    CHECK (purpose IN ('event_invitations', 'recruitment_pool', 'promotional'));

ALTER TABLE contact_consents DROP CONSTRAINT IF EXISTS contact_consents_source_check;
ALTER TABLE contact_consents ADD CONSTRAINT contact_consents_source_check
    CHECK (source IN ('guest_apply', 'forms', 'place', 'guessr', 'self', 'signup'));
