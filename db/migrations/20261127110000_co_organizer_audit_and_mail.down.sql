SET LOCAL lock_timeout = '5s';

-- Puts back 20261126110000's group checks, without the Group-less
-- co_organizer.* row of an Event without an Owner team. They are added NOT
-- VALID so that the rows already written stay (the record is kept three
-- years); a new one is refused. The action check stays wide, as the
-- earlier migrations' downs leave it.
--
-- The mail queue stays: dropping it would lose the mails still waiting.
-- The release before neither reads nor writes it, and the up migration
-- finds it again (IF NOT EXISTS).
ALTER TABLE team_audit_records DROP CONSTRAINT IF EXISTS team_audit_records_group_id_check;
ALTER TABLE team_audit_records ADD CONSTRAINT team_audit_records_group_id_check CHECK (
    group_id IS NOT NULL
    OR (action = 'team.create' AND outcome <> 'ok')
    OR (action IN ('app.assign', 'app.unassign') AND target_user_id IS NOT NULL)
    OR (action LIKE 'user.%' AND target_user_id IS NOT NULL)
    OR action LIKE 'keycloak.%') NOT VALID;

ALTER TABLE team_audit_records DROP CONSTRAINT IF EXISTS team_audit_records_group_path_check;
ALTER TABLE team_audit_records ADD CONSTRAINT team_audit_records_group_path_check CHECK (
    (action NOT LIKE 'user.%' AND action NOT LIKE 'keycloak.%' AND group_path LIKE '/%' AND char_length(group_path) <= 1024)
    OR (action IN ('app.assign', 'app.unassign') AND group_path = '' AND group_id IS NULL)
    OR ((action LIKE 'user.%' OR action LIKE 'keycloak.%') AND group_path = '' AND group_id IS NULL)) NOT VALID;
