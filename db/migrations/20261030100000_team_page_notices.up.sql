-- The team page notice (docs/team-page-consent.md, ekip-yetki-devri 21):
-- the one mail a member listed on a public team page, who has not decided
-- about the team page consent, gets when the transition starts. A person
-- with a row here was sent it (or it was sent without an answer) and is
-- never sent it again. Ids only: the address is read when the mail goes.
-- Account erasure deletes the person's row.
--
-- A new table only: the release before this one never reads it.
SET LOCAL lock_timeout = '5s';

CREATE TABLE IF NOT EXISTS team_page_notices (
    user_id UUID PRIMARY KEY,
    sent_at TIMESTAMPTZ NOT NULL
);
