SET LOCAL lock_timeout = '5s';

-- Walk-in at the door ("Kapıda kayıt", docs/walk-in.md): who may take
-- check-ins at an Event's door writes the Ticket of a person who has none,
-- and in the venue Entry mode their entry, in one request.
--
-- tickets takes the guest's optional university, department and grade
-- (year of study), for the Event's statistics: empty for none, as the
-- other guest_* columns. A grade is one of the fixed keys.
--
-- venue_entries takes the source walk_in: the entry of a Ticket the door
-- registered.
--
-- The Team audit record takes ticket.walk_in, and the basis door_staff
-- (the Event's door staff, no Leader of its team) for that action alone,
-- through the Event's team. The checks keep 20261127115000's clauses.
--
-- The release before runs on the new schema: it neither reads nor writes
-- the new columns (they have defaults) and writes none of the new rows.
ALTER TABLE tickets ADD COLUMN IF NOT EXISTS guest_university TEXT NOT NULL DEFAULT '';
ALTER TABLE tickets ADD COLUMN IF NOT EXISTS guest_department TEXT NOT NULL DEFAULT '';
ALTER TABLE tickets ADD COLUMN IF NOT EXISTS guest_grade TEXT NOT NULL DEFAULT '';
ALTER TABLE tickets DROP CONSTRAINT IF EXISTS tickets_guest_university_check;
ALTER TABLE tickets ADD CONSTRAINT tickets_guest_university_check CHECK (char_length(guest_university) <= 200);
ALTER TABLE tickets DROP CONSTRAINT IF EXISTS tickets_guest_department_check;
ALTER TABLE tickets ADD CONSTRAINT tickets_guest_department_check CHECK (char_length(guest_department) <= 200);
ALTER TABLE tickets DROP CONSTRAINT IF EXISTS tickets_guest_grade_check;
ALTER TABLE tickets ADD CONSTRAINT tickets_guest_grade_check CHECK (
    guest_grade IN ('', 'prep', '1', '2', '3', '4', 'masters', 'phd', 'graduate', 'other'));

ALTER TABLE venue_entries DROP CONSTRAINT IF EXISTS venue_entries_source_check;
ALTER TABLE venue_entries ADD CONSTRAINT venue_entries_source_check CHECK (source IN ('scan', 'desk', 'attendance', 'walk_in'));

ALTER TABLE team_audit_records DROP CONSTRAINT IF EXISTS team_audit_records_action_check;
ALTER TABLE team_audit_records ADD CONSTRAINT team_audit_records_action_check CHECK (action IN (
    'member.add', 'member.remove', 'leader.add', 'leader.remove',
    'team.create', 'team.code', 'team.profile', 'team.move', 'team.delete',
    'group.roles', 'group.attributes', 'app.assign', 'app.unassign',
    'membership.term.extend', 'membership.term.handover', 'membership.term.expired',
    'ticket.resend', 'ticket.link.renew', 'ticket.cancel', 'ticket.restore', 'ticket.promote',
    'ticket.import', 'ticket.import.preview', 'ticket.walk_in',
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
            'ticket.resend', 'ticket.link.renew', 'co_organizer.add'))
    OR (actor_basis = 'door_staff' AND actor_via_group_id IS NOT NULL AND action = 'ticket.walk_in')
    OR (actor_basis = 'console' AND actor_via_group_id IS NULL AND via = 'console' AND outcome = 'ok'));
