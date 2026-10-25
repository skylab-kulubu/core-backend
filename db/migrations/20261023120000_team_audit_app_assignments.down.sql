-- Puts back the checks without the app actions. They are added NOT VALID so
-- that app rows already written stay (the record is kept three years).
DROP INDEX IF EXISTS team_audit_records_app_idx;

ALTER TABLE team_audit_records DROP CONSTRAINT IF EXISTS team_audit_records_action_check;
ALTER TABLE team_audit_records ADD CONSTRAINT team_audit_records_action_check CHECK (action IN (
    'member.add', 'member.remove', 'leader.add', 'leader.remove',
    'team.create', 'team.code', 'team.profile', 'team.move', 'team.delete',
    'group.roles', 'group.attributes')) NOT VALID;

ALTER TABLE team_audit_records DROP CONSTRAINT IF EXISTS team_audit_records_group_id_check;
ALTER TABLE team_audit_records ADD CONSTRAINT team_audit_records_group_id_check CHECK (
    group_id IS NOT NULL OR (action = 'team.create' AND outcome <> 'ok')) NOT VALID;

ALTER TABLE team_audit_records DROP CONSTRAINT IF EXISTS team_audit_records_group_path_check;
ALTER TABLE team_audit_records ADD CONSTRAINT team_audit_records_group_path_check CHECK (
    group_path LIKE '/%' AND char_length(group_path) <= 1024) NOT VALID;

ALTER TABLE team_audit_records DROP CONSTRAINT IF EXISTS team_audit_records_target_check;
ALTER TABLE team_audit_records ADD CONSTRAINT team_audit_records_target_check CHECK (
    (action IN ('member.add', 'member.remove', 'leader.add', 'leader.remove')) = (target_user_id IS NOT NULL)) NOT VALID;
