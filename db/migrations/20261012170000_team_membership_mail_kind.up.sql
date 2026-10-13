-- Membership mail for every Group (Yusuf, 2026-10-12;
-- docs/team-membership-mail.md): a row says which mail it was queued for.
-- team (club.team-membership), role (a Privileged Group:
-- club.role-membership) or group (any other Group: club.group-membership).
-- The sending core's rule decides the mail a row gets; the column says what
-- waits. Rows already queued, and rows an older core writes during a
-- rollout, are team rows, as that core only queued teams.
ALTER TABLE team_membership_mails ADD COLUMN IF NOT EXISTS kind TEXT NOT NULL DEFAULT 'team';
ALTER TABLE team_membership_mails DROP CONSTRAINT IF EXISTS team_membership_mails_kind_check;
ALTER TABLE team_membership_mails ADD CONSTRAINT team_membership_mails_kind_check CHECK (kind IN ('team', 'role', 'group'));
