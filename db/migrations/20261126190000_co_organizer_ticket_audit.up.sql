SET LOCAL lock_timeout = '5s';

-- A Ticket's mail sent again and its link renewed by an Event's
-- Co-organizer (docs/co-organizers.md): who may give the Event's Tickets
-- out as one of its Co-organizer people, and is no Leader of a Team the
-- Event's team sits in or under, is recorded as an event editor through the
-- Event's team, as a Ticket's cancellation already is. The basis check
-- keeps 20261126110000's clauses and adds ticket.resend and
-- ticket.link.renew to event_editor's actions.
--
-- The release before this one runs on the new schema: it writes none of
-- the new rows.
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
            'ticket.resend', 'ticket.link.renew'))
    OR (actor_basis = 'console' AND actor_via_group_id IS NULL AND via = 'console' AND outcome = 'ok'));
