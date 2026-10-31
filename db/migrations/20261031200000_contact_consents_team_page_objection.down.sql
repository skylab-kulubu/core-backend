-- Puts back the purpose check without the team page objection. It is added
-- NOT VALID so that the objections already written stay: they are records
-- (proof). The team_page rows were never changed, so a core of before this
-- migration hides who withdrew the consent as it did.
SET LOCAL lock_timeout = '5s';

ALTER TABLE contact_consents DROP CONSTRAINT IF EXISTS contact_consents_purpose_check;
ALTER TABLE contact_consents ADD CONSTRAINT contact_consents_purpose_check
    CHECK (purpose IN ('event_invitations', 'recruitment_pool', 'promotional', 'team_page')) NOT VALID;
