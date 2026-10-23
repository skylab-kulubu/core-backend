-- App assignments (ekip-yetkileri ticket 15, docs/app-assignments.md): the
-- panel gives an app role to a Group or a person and takes it off, and each
-- change is a row: app.assign or app.unassign. Its target is a Group
-- (group_id, group_path) or a person (target_user_id, group_path empty),
-- never both; after names the client and the role, and the caller's
-- optional note (at most 200 characters; the panel asks for no personal
-- data in it). Kept as every row is (team_audit_age).
ALTER TABLE team_audit_records DROP CONSTRAINT IF EXISTS team_audit_records_action_check;
ALTER TABLE team_audit_records ADD CONSTRAINT team_audit_records_action_check CHECK (action IN (
    'member.add', 'member.remove', 'leader.add', 'leader.remove',
    'team.create', 'team.code', 'team.profile', 'team.move', 'team.delete',
    'group.roles', 'group.attributes', 'app.assign', 'app.unassign'));

ALTER TABLE team_audit_records DROP CONSTRAINT IF EXISTS team_audit_records_group_id_check;
ALTER TABLE team_audit_records ADD CONSTRAINT team_audit_records_group_id_check CHECK (
    group_id IS NOT NULL
    OR (action = 'team.create' AND outcome <> 'ok')
    OR (action IN ('app.assign', 'app.unassign') AND target_user_id IS NOT NULL));

ALTER TABLE team_audit_records DROP CONSTRAINT IF EXISTS team_audit_records_group_path_check;
ALTER TABLE team_audit_records ADD CONSTRAINT team_audit_records_group_path_check CHECK (
    (group_path LIKE '/%' AND char_length(group_path) <= 1024)
    OR (action IN ('app.assign', 'app.unassign') AND group_path = '' AND group_id IS NULL));

ALTER TABLE team_audit_records DROP CONSTRAINT IF EXISTS team_audit_records_target_check;
ALTER TABLE team_audit_records ADD CONSTRAINT team_audit_records_target_check CHECK (
    CASE
        WHEN action IN ('member.add', 'member.remove', 'leader.add', 'leader.remove') THEN target_user_id IS NOT NULL
        WHEN action IN ('app.assign', 'app.unassign') THEN (target_user_id IS NULL) <> (group_id IS NULL)
        ELSE target_user_id IS NULL
    END);

-- The app's rows (the panel's "Denetim" tab, newest first) and its latest
-- per target and role.
CREATE INDEX IF NOT EXISTS team_audit_records_app_idx ON team_audit_records ((after->>'client'), occurred_at DESC)
    WHERE action IN ('app.assign', 'app.unassign');
