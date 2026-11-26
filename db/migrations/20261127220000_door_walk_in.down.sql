SET LOCAL lock_timeout = '5s';

-- Puts back 20261127115000's audit checks and 20261124150000's entry
-- source check, without ticket.walk_in, door_staff and walk_in. They are
-- added NOT VALID so that the rows a Walk-in already wrote stay (the
-- audit record is kept three years; an entry until its retention).
--
-- The tickets' guest_university, guest_department and guest_grade stay
-- with their values and checks: the release before neither reads nor
-- writes them (they have defaults), and the next release reads them again
-- (the up migration finds them, ADD COLUMN IF NOT EXISTS). To remove them
-- for good, by hand: ALTER TABLE tickets DROP CONSTRAINT
-- tickets_guest_university_check, DROP CONSTRAINT
-- tickets_guest_department_check, DROP CONSTRAINT tickets_guest_grade_check,
-- DROP COLUMN guest_university, DROP COLUMN guest_department, DROP COLUMN
-- guest_grade;
ALTER TABLE venue_entries DROP CONSTRAINT IF EXISTS venue_entries_source_check;
ALTER TABLE venue_entries ADD CONSTRAINT venue_entries_source_check
    CHECK (source IN ('scan', 'desk', 'attendance')) NOT VALID;

ALTER TABLE team_audit_records DROP CONSTRAINT IF EXISTS team_audit_records_action_check;
ALTER TABLE team_audit_records ADD CONSTRAINT team_audit_records_action_check CHECK (action IN (
    'member.add', 'member.remove', 'leader.add', 'leader.remove',
    'team.create', 'team.code', 'team.profile', 'team.move', 'team.delete',
    'group.roles', 'group.attributes', 'app.assign', 'app.unassign',
    'membership.term.extend', 'membership.term.handover', 'membership.term.expired',
    'ticket.resend', 'ticket.link.renew', 'ticket.cancel', 'ticket.restore', 'ticket.promote',
    'ticket.import', 'ticket.import.preview',
    'co_organizer.add', 'co_organizer.remove', 'co_organizer.change',
    'user.enable', 'user.disable', 'user.password.set', 'user.password.mail',
    'user.email.change', 'user.email.verify', 'user.email.mail', 'user.email.primary',
    'user.required_actions', 'user.credential.remove', 'user.idp.unlink',
    'user.lockout.clear', 'user.profile', 'user.roles',
    'keycloak.client', 'keycloak.role')) NOT VALID;

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
            'ticket.resend', 'ticket.link.renew', 'co_organizer.add'))
    OR (actor_basis = 'console' AND actor_via_group_id IS NULL AND via = 'console' AND outcome = 'ok')) NOT VALID;
