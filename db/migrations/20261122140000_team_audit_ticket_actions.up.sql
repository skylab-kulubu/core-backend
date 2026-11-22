-- Sending a Ticket's mail again and renewing its link (bilet-qr-ve-pdf
-- ticket 05; docs/ticket-resend.md) are recorded in the Team audit record:
-- ticket.resend and ticket.link.renew, by the Leader of the Event's team or
-- a Privileged caller, against the Event's team (group_id, group_path).
-- after carries the Ticket's and the Event's ids, never a person; the
-- record has no target person.
ALTER TABLE team_audit_records DROP CONSTRAINT IF EXISTS team_audit_records_action_check;
ALTER TABLE team_audit_records ADD CONSTRAINT team_audit_records_action_check CHECK (action IN (
    'member.add', 'member.remove', 'leader.add', 'leader.remove',
    'team.create', 'team.code', 'team.profile', 'team.move', 'team.delete',
    'group.roles', 'group.attributes', 'app.assign', 'app.unassign',
    'membership.term.extend', 'membership.term.handover', 'membership.term.expired',
    'ticket.resend', 'ticket.link.renew'));
