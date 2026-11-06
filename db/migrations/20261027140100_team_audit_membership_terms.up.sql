-- Management membership terms (docs/management-membership-terms.md,
-- grup-yonetimi 11) write three actions to the Team audit record:
-- membership.term.extend and membership.term.handover by a person, and
-- membership.term.expired by core's daily job. The job's rows carry
-- actor_basis 'system' with the nil subject as actor_id; client names the
-- job (system:term-expiry). Each targets the person whose term it is.
ALTER TABLE team_audit_records DROP CONSTRAINT IF EXISTS team_audit_records_action_check;
ALTER TABLE team_audit_records ADD CONSTRAINT team_audit_records_action_check CHECK (action IN (
    'member.add', 'member.remove', 'leader.add', 'leader.remove',
    'team.create', 'team.code', 'team.profile', 'team.move', 'team.delete',
    'group.roles', 'group.attributes', 'app.assign', 'app.unassign',
    'membership.term.extend', 'membership.term.handover', 'membership.term.expired'));

ALTER TABLE team_audit_records DROP CONSTRAINT IF EXISTS team_audit_records_actor_basis_check;
ALTER TABLE team_audit_records ADD CONSTRAINT team_audit_records_actor_basis_check CHECK (
    (actor_basis = 'privileged' AND actor_via_group_id IS NULL)
    OR (actor_basis = 'leader' AND actor_via_group_id IS NOT NULL)
    OR (actor_basis = 'none' AND actor_via_group_id IS NULL AND outcome = 'denied')
    OR (actor_basis = 'system' AND actor_via_group_id IS NULL
        AND actor_id = '00000000-0000-0000-0000-000000000000'
        AND action = 'membership.term.expired'));

ALTER TABLE team_audit_records DROP CONSTRAINT IF EXISTS team_audit_records_target_check;
ALTER TABLE team_audit_records ADD CONSTRAINT team_audit_records_target_check CHECK (
    CASE
        WHEN action IN ('member.add', 'member.remove', 'leader.add', 'leader.remove',
            'membership.term.extend', 'membership.term.handover', 'membership.term.expired') THEN target_user_id IS NOT NULL
        WHEN action IN ('app.assign', 'app.unassign') THEN (target_user_id IS NULL) <> (group_id IS NULL)
        ELSE target_user_id IS NULL
    END);
