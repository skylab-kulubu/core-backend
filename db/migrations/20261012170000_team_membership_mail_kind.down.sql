-- Rows of the role and group mails are left: an older core sends them as
-- it would any row whose Group is no team (skipped_not_team).
ALTER TABLE team_membership_mails DROP CONSTRAINT IF EXISTS team_membership_mails_kind_check;
ALTER TABLE team_membership_mails DROP COLUMN IF EXISTS kind;
