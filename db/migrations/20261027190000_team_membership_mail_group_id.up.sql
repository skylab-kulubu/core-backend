-- The Group a queued membership mail is about, by its Keycloak id (ADR 0065
-- ticket 07): a Team moved or given a new code within the 72 hours a row
-- waits is found by its id, so the mail names it, not the last segment of
-- a path that no longer exists. Rows queued before, and rows an older core
-- writes during a rollout, have none and are read by their path as before.
ALTER TABLE team_membership_mails ADD COLUMN IF NOT EXISTS group_id UUID;
