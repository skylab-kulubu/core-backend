-- An Event's external registration link and the attendee import
-- (docs/attendee-import.md).
--
-- external_registration_url is where people register for an Event whose
-- sign-up runs on another product's form (Microsoft Forms and the like):
-- an https address, '' for none. The site's "Kayıt ol" opens it. The check
-- repeats what core checks before it writes (event.ValidExternalRegistrationURL).
ALTER TABLE events ADD COLUMN IF NOT EXISTS external_registration_url TEXT NOT NULL DEFAULT '';
ALTER TABLE events DROP CONSTRAINT IF EXISTS events_external_registration_url_check;
ALTER TABLE events ADD CONSTRAINT events_external_registration_url_check CHECK (
    external_registration_url = ''
    OR (external_registration_url LIKE 'https://_%' AND char_length(external_registration_url) <= 2048));

-- An operator's import of a list of attendees is one audit row
-- (ticket.import), its preview another (ticket.import.preview): the Event's
-- team, the Event and the counts, never a name or an e-mail address. An
-- Event's editor may make them, as a cancellation.
ALTER TABLE team_audit_records DROP CONSTRAINT IF EXISTS team_audit_records_action_check;
ALTER TABLE team_audit_records ADD CONSTRAINT team_audit_records_action_check CHECK (action IN (
    'member.add', 'member.remove', 'leader.add', 'leader.remove',
    'team.create', 'team.code', 'team.profile', 'team.move', 'team.delete',
    'group.roles', 'group.attributes', 'app.assign', 'app.unassign',
    'membership.term.extend', 'membership.term.handover', 'membership.term.expired',
    'ticket.resend', 'ticket.link.renew', 'ticket.cancel', 'ticket.restore', 'ticket.promote',
    'ticket.import', 'ticket.import.preview',
    'user.enable', 'user.disable', 'user.password.set', 'user.password.mail',
    'user.email.change', 'user.email.verify', 'user.email.mail', 'user.email.primary',
    'user.required_actions', 'user.credential.remove', 'user.idp.unlink',
    'user.lockout.clear', 'user.profile'));

ALTER TABLE team_audit_records DROP CONSTRAINT IF EXISTS team_audit_records_actor_basis_check;
ALTER TABLE team_audit_records ADD CONSTRAINT team_audit_records_actor_basis_check CHECK (
    (actor_basis = 'privileged' AND actor_via_group_id IS NULL)
    OR (actor_basis = 'leader' AND actor_via_group_id IS NOT NULL)
    OR (actor_basis = 'none' AND actor_via_group_id IS NULL AND outcome = 'denied')
    OR (actor_basis = 'system' AND actor_via_group_id IS NULL
        AND actor_id = '00000000-0000-0000-0000-000000000000'
        AND action = 'membership.term.expired')
    OR (actor_basis = 'event_editor' AND actor_via_group_id IS NOT NULL
        AND action IN ('ticket.cancel', 'ticket.restore', 'ticket.promote', 'ticket.import', 'ticket.import.preview')));
