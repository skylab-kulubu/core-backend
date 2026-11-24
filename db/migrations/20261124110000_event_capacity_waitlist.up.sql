-- An Event's capacity and waiting list (kontenjan-iptal-hatirlatma 06, 07;
-- docs/event-capacity.md).
--
-- events.waitlist_enabled: once the Event's active Tickets fill its
-- capacity (capacity > 0), a registration goes on its waiting list (a
-- waitlisted Ticket) while this is on; off, a full Event takes nobody new
-- but who an operator adds. Every Event already there has it on (the
-- default), as a new one does.
--
-- ticket_notifications.kind is which mail a queued row sends: created (the
-- "Biletin hazır" mail, every row before this one), waitlisted ("Bekleme
-- listesindesin") or promoted ("Yer açıldı, biletin hazır").
--
-- ticket.promote is an operator moving a waitlisted Ticket to active from
-- the panel, recorded as ticket.cancel is (the event_editor basis too).
--
-- The release before this one runs on the new schema: it writes neither
-- column (the defaults are what it means), and its worker sends every queued
-- row as "Biletin hazır", which only the new release queues otherwise.
SET LOCAL lock_timeout = '5s';

ALTER TABLE events ADD COLUMN IF NOT EXISTS waitlist_enabled BOOLEAN NOT NULL DEFAULT true;
COMMENT ON COLUMN events.waitlist_enabled IS 'A full Event (capacity > 0) puts a registration on its waiting list while on.';

ALTER TABLE ticket_notifications ADD COLUMN IF NOT EXISTS kind TEXT NOT NULL DEFAULT 'created';
ALTER TABLE ticket_notifications DROP CONSTRAINT IF EXISTS ticket_notifications_kind_check;
ALTER TABLE ticket_notifications ADD CONSTRAINT ticket_notifications_kind_check
    CHECK (kind IN ('created', 'waitlisted', 'promoted'));

-- The Event's waiting list in its order: who waited longest is promoted
-- first.
CREATE INDEX IF NOT EXISTS tickets_waitlist_idx ON tickets (event_id, waitlisted_at, created_at)
    WHERE status = 'waitlisted';

ALTER TABLE team_audit_records DROP CONSTRAINT IF EXISTS team_audit_records_action_check;
ALTER TABLE team_audit_records ADD CONSTRAINT team_audit_records_action_check CHECK (action IN (
    'member.add', 'member.remove', 'leader.add', 'leader.remove',
    'team.create', 'team.code', 'team.profile', 'team.move', 'team.delete',
    'group.roles', 'group.attributes', 'app.assign', 'app.unassign',
    'membership.term.extend', 'membership.term.handover', 'membership.term.expired',
    'ticket.resend', 'ticket.link.renew', 'ticket.cancel', 'ticket.restore', 'ticket.promote',
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
        AND action IN ('ticket.cancel', 'ticket.restore', 'ticket.promote')));
