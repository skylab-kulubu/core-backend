-- The team page consent (docs/team-page-consent.md, aydınlatma s6 row 4 b):
-- a member's consent to be shown on their teams' public pages. It is kept
-- with the other consents for its evidence (purpose, text version, source,
-- times), its proof after withdrawal and its erasure, but it allows
-- publishing on the web, not mails: channel web.
SET LOCAL lock_timeout = '5s';

ALTER TABLE contact_consents DROP CONSTRAINT IF EXISTS contact_consents_purpose_check;
ALTER TABLE contact_consents ADD CONSTRAINT contact_consents_purpose_check
    CHECK (purpose IN ('event_invitations', 'recruitment_pool', 'promotional', 'team_page'));

ALTER TABLE contact_consents DROP CONSTRAINT IF EXISTS contact_consents_channel_check;
ALTER TABLE contact_consents ADD CONSTRAINT contact_consents_channel_check
    CHECK (channel IN ('email', 'web'));
