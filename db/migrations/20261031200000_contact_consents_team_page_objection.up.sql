-- The team page objection (docs/team-page-consent.md, aydınlatma s6 row 4 b;
-- Yusuf, 2026-10-31, lawyer approved): every member is shown on their teams'
-- public pages unless they object, and an open grant of purpose
-- team_page_objection is that objection. It is kept with the consents for
-- the same evidence (text version, source, client, times), proof after it
-- ends and erasure; channel web.
--
-- The team_page consent (2026-10-27 to 2026-10-31) is no longer taken. A
-- member who withdrew it asked not to be shown: each active account whose
-- team_page grants have all ended, and who has no objection row yet, gets
-- an open objection from the time of the last withdrawal, on the text they
-- withdrew (ekip-sayfasi-v1). Their team_page rows stay as they are
-- (proof). Running it again adds nothing.
SET LOCAL lock_timeout = '5s';

ALTER TABLE contact_consents DROP CONSTRAINT IF EXISTS contact_consents_purpose_check;
ALTER TABLE contact_consents ADD CONSTRAINT contact_consents_purpose_check
    CHECK (purpose IN ('event_invitations', 'recruitment_pool', 'promotional', 'team_page', 'team_page_objection'));

INSERT INTO contact_consents (
    id, purpose, channel, user_id, source, client_id, text_version, granted_at, confirmed_at, confirmed_via
)
SELECT gen_random_uuid(), 'team_page_objection', 'web', c.user_id, 'self', '', 'ekip-sayfasi-v1',
    max(c.ended_at), max(c.ended_at), 'account'
FROM contact_consents c
JOIN users u ON u.id = c.user_id AND u.account_state = 'active'
WHERE c.purpose = 'team_page'
  AND NOT EXISTS (SELECT 1 FROM account_deletion_requests d WHERE d.subject_id = c.user_id)
  AND NOT EXISTS (
    SELECT 1 FROM contact_consents o WHERE o.purpose = 'team_page_objection' AND o.user_id = c.user_id
  )
GROUP BY c.user_id
HAVING bool_and(c.ended_at IS NOT NULL);
