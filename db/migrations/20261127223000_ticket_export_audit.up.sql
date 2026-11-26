SET LOCAL lock_timeout = '5s';

-- An attendee export in the Team audit record (docs/attendee-export.md).
--
-- The record takes ticket.export: a download of an Event's attendees (a
-- report, its format and its row count; never a row's content), recorded
-- as ticket.import is: the Event's Owner team, and an event editor's basis
-- for a reader who is neither Privileged nor a Leader. The other checks
-- stay 20261127220000's (door_walk_in), ticket.walk_in and the
-- door_staff basis included.
--
-- The release before this one runs on the new schema: it writes no
-- ticket.export row.
ALTER TABLE team_audit_records DROP CONSTRAINT IF EXISTS team_audit_records_action_check;
ALTER TABLE team_audit_records ADD CONSTRAINT team_audit_records_action_check CHECK (action IN (
    'member.add', 'member.remove', 'leader.add', 'leader.remove',
    'team.create', 'team.code', 'team.profile', 'team.move', 'team.delete',
    'group.roles', 'group.attributes', 'app.assign', 'app.unassign',
    'membership.term.extend', 'membership.term.handover', 'membership.term.expired',
    'ticket.resend', 'ticket.link.renew', 'ticket.cancel', 'ticket.restore', 'ticket.promote',
    'ticket.import', 'ticket.import.preview', 'ticket.walk_in', 'ticket.export',
    'co_organizer.add', 'co_organizer.remove', 'co_organizer.change',
    'user.enable', 'user.disable', 'user.password.set', 'user.password.mail',
    'user.email.change', 'user.email.verify', 'user.email.mail', 'user.email.primary',
    'user.required_actions', 'user.credential.remove', 'user.idp.unlink',
    'user.lockout.clear', 'user.profile', 'user.roles',
    'keycloak.client', 'keycloak.role'));

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
            'ticket.export', 'ticket.resend', 'ticket.link.renew', 'co_organizer.add'))
    OR (actor_basis = 'door_staff' AND actor_via_group_id IS NOT NULL AND action = 'ticket.walk_in')
    OR (actor_basis = 'console' AND actor_via_group_id IS NULL AND via = 'console' AND outcome = 'ok'));
