-- A person's account changed from the panel's person card
-- (docs/user-accounts.md) is recorded in the Team audit record: the user.*
-- actions. The target is the person (target_user_id), with no Group
-- (group_id NULL, group_path ''). before and after carry flags and ids only
-- (enabled, temporary, verified, primary, required action aliases, a
-- credential's id and type, a provider's alias, changed field names), never
-- an address, a name or a password. Kept as every row is (team_audit_age).
ALTER TABLE team_audit_records DROP CONSTRAINT IF EXISTS team_audit_records_action_check;
ALTER TABLE team_audit_records ADD CONSTRAINT team_audit_records_action_check CHECK (action IN (
    'member.add', 'member.remove', 'leader.add', 'leader.remove',
    'team.create', 'team.code', 'team.profile', 'team.move', 'team.delete',
    'group.roles', 'group.attributes', 'app.assign', 'app.unassign',
    'membership.term.extend', 'membership.term.handover', 'membership.term.expired',
    'ticket.resend', 'ticket.link.renew',
    'user.enable', 'user.disable', 'user.password.set', 'user.password.mail',
    'user.email.change', 'user.email.verify', 'user.email.mail', 'user.email.primary',
    'user.required_actions', 'user.credential.remove', 'user.idp.unlink',
    'user.lockout.clear', 'user.profile'));

ALTER TABLE team_audit_records DROP CONSTRAINT IF EXISTS team_audit_records_group_id_check;
ALTER TABLE team_audit_records ADD CONSTRAINT team_audit_records_group_id_check CHECK (
    group_id IS NOT NULL
    OR (action = 'team.create' AND outcome <> 'ok')
    OR (action IN ('app.assign', 'app.unassign') AND target_user_id IS NOT NULL)
    OR (action LIKE 'user.%' AND target_user_id IS NOT NULL));

ALTER TABLE team_audit_records DROP CONSTRAINT IF EXISTS team_audit_records_group_path_check;
ALTER TABLE team_audit_records ADD CONSTRAINT team_audit_records_group_path_check CHECK (
    (action NOT LIKE 'user.%' AND group_path LIKE '/%' AND char_length(group_path) <= 1024)
    OR (action IN ('app.assign', 'app.unassign') AND group_path = '' AND group_id IS NULL)
    OR (action LIKE 'user.%' AND group_path = '' AND group_id IS NULL));

ALTER TABLE team_audit_records DROP CONSTRAINT IF EXISTS team_audit_records_target_check;
ALTER TABLE team_audit_records ADD CONSTRAINT team_audit_records_target_check CHECK (
    CASE
        WHEN action IN ('member.add', 'member.remove', 'leader.add', 'leader.remove',
            'membership.term.extend', 'membership.term.handover', 'membership.term.expired') THEN target_user_id IS NOT NULL
        WHEN action IN ('app.assign', 'app.unassign') THEN (target_user_id IS NULL) <> (group_id IS NULL)
        WHEN action LIKE 'user.%' THEN target_user_id IS NOT NULL AND group_id IS NULL
        ELSE target_user_id IS NULL
    END);

-- A person's account rows, newest first (the person card's history).
CREATE INDEX IF NOT EXISTS team_audit_records_user_idx ON team_audit_records (target_user_id, occurred_at DESC)
    WHERE action LIKE 'user.%';
