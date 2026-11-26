SET LOCAL lock_timeout = '5s';

-- Puts back 20261126110000's group checks, without the Group-less
-- co_organizer.* row of an Event without an Owner team. They are added NOT
-- VALID so that the rows already written stay (the record is kept three
-- years); a new one is refused. The action check stays wide, as the
-- earlier migrations' downs leave it.
--
-- The basis check goes back to 20261126190000's, without an event editor's
-- co_organizer.add, NOT VALID too.
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

ALTER TABLE team_audit_records DROP CONSTRAINT IF EXISTS team_audit_records_actor_basis_check;
ALTER TABLE team_audit_records ADD CONSTRAINT team_audit_records_actor_basis_check CHECK (
    (actor_basis = 'privileged' AND actor_via_group_id IS NULL)
    OR (actor_basis = 'leader' AND actor_via_group_id IS NOT NULL)
    OR (actor_basis = 'none' AND actor_via_group_id IS NULL AND outcome = 'denied')
    OR (actor_basis = 'system' AND actor_via_group_id IS NULL
        AND actor_id = '00000000-0000-0000-0000-000000000000'
        AND action IN ('membership.term.expired', 'member.remove'))
    OR (actor_basis = 'event_editor' AND actor_via_group_id IS NOT NULL
        AND action IN ('ticket.cancel', 'ticket.restore', 'ticket.promote', 'ticket.import', 'ticket.import.preview',
            'ticket.resend', 'ticket.link.renew'))
    OR (actor_basis = 'console' AND actor_via_group_id IS NULL AND via = 'console' AND outcome = 'ok')) NOT VALID;
